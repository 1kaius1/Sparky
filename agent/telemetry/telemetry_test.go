// SPDX-License-Identifier: AGPL-3.0-or-later

package telemetry

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "procfile")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	return path
}

func newTestCollector(t *testing.T, run commandRunner, procStat, procMeminfo string) *Collector {
	t.Helper()
	c := &Collector{runCommand: run}
	if procStat != "" {
		c.procStatPath = writeFile(t, procStat)
	}
	if procMeminfo != "" {
		c.procMeminfoPath = writeFile(t, procMeminfo)
	}
	return c
}

func fakeNvidiaSMI(output string, err error) commandRunner {
	return func(context.Context, string, ...string) ([]byte, error) {
		return []byte(output), err
	}
}

const sampleMeminfo = `MemTotal:       16186088 kB
MemFree:          776592 kB
MemAvailable:   10840668 kB
Buffers:         2443948 kB
Cached:          5499228 kB
`

func TestCollector_Read_SingleGPU(t *testing.T) {
	procStat := "cpu  310703 190 114282 3059121 21317 0 5464 0 0 0\n"
	c := newTestCollector(t, fakeNvidiaSMI("0, 45, 8192, 24576", nil), procStat, sampleMeminfo)

	reading, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if len(reading.GPUs) != 1 {
		t.Fatalf("len(GPUs) = %d, want 1", len(reading.GPUs))
	}
	if reading.GPUs[0].Index != 0 {
		t.Errorf("GPUs[0].Index = %v, want 0", reading.GPUs[0].Index)
	}
	if reading.GPUs[0].UtilizationPct != 45 {
		t.Errorf("GPUs[0].UtilizationPct = %v, want 45", reading.GPUs[0].UtilizationPct)
	}
	if reading.GPUs[0].MemoryUsedMB != 8192 || reading.GPUs[0].MemoryTotalMB != 24576 {
		t.Errorf("GPUs[0].MemoryUsedMB/MemoryTotalMB = %v/%v, want 8192/24576", reading.GPUs[0].MemoryUsedMB, reading.GPUs[0].MemoryTotalMB)
	}
	// First call from a fresh Collector - no prior /proc/stat sample yet.
	if reading.CPUUtilizationPct != 0 {
		t.Errorf("CPUUtilizationPct on first read = %v, want 0", reading.CPUUtilizationPct)
	}
	wantTotalMB := float64(16186088) / 1024
	wantUsedMB := float64(16186088-10840668) / 1024
	if reading.SystemMemoryTotalMB != wantTotalMB {
		t.Errorf("SystemMemoryTotalMB = %v, want %v", reading.SystemMemoryTotalMB, wantTotalMB)
	}
	if reading.SystemMemoryUsedMB != wantUsedMB {
		t.Errorf("SystemMemoryUsedMB = %v, want %v", reading.SystemMemoryUsedMB, wantUsedMB)
	}
}

func TestCollector_Read_MultipleGPUs_PerGPU(t *testing.T) {
	procStat := "cpu  0 0 0 0 0 0 0 0 0 0\n"
	// Two GPUs, indices 0 and 1 - no summing/averaging, one GPUReading each.
	c := newTestCollector(t, fakeNvidiaSMI("0, 40, 4096, 24576\n1, 60, 6144, 24576", nil), procStat, sampleMeminfo)

	reading, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if len(reading.GPUs) != 2 {
		t.Fatalf("len(GPUs) = %d, want 2", len(reading.GPUs))
	}
	want := []GPUReading{
		{Index: 0, UtilizationPct: 40, MemoryUsedMB: 4096, MemoryTotalMB: 24576},
		{Index: 1, UtilizationPct: 60, MemoryUsedMB: 6144, MemoryTotalMB: 24576},
	}
	for i, w := range want {
		if reading.GPUs[i] != w {
			t.Errorf("GPUs[%d] = %+v, want %+v", i, reading.GPUs[i], w)
		}
	}
}

func TestCollector_Read_NvidiaSMIFails(t *testing.T) {
	procStat := "cpu  0 0 0 0 0 0 0 0 0 0\n"
	c := newTestCollector(t, fakeNvidiaSMI("", errors.New("executable file not found")), procStat, sampleMeminfo)

	_, err := c.Read(context.Background())
	if err == nil {
		t.Fatal("Read() succeeded despite nvidia-smi failing")
	}
}

func TestCollector_Read_NvidiaSMIEmptyOutput(t *testing.T) {
	procStat := "cpu  0 0 0 0 0 0 0 0 0 0\n"
	c := newTestCollector(t, fakeNvidiaSMI("", nil), procStat, sampleMeminfo)

	_, err := c.Read(context.Background())
	if err == nil {
		t.Fatal("Read() succeeded despite nvidia-smi reporting no GPUs")
	}
}

func TestCollector_ReadCPU_SecondCallComputesRealDelta(t *testing.T) {
	c := newTestCollector(t, fakeNvidiaSMI("0, 0, 0, 1", nil), "", sampleMeminfo)
	c.procStatPath = writeFile(t, "cpu  1000 0 0 9000 0 0 0 0 0 0\n") // 10000 total, 9000 idle

	if _, err := c.Read(context.Background()); err != nil {
		t.Fatalf("first Read() error: %v", err)
	}

	// Second sample: +1000 total ticks, all of it busy (idle unchanged) ->
	// 100% utilization over the delta.
	if err := os.WriteFile(c.procStatPath, []byte("cpu  2000 0 0 9000 0 0 0 0 0 0\n"), 0o600); err != nil {
		t.Fatalf("update /proc/stat fixture: %v", err)
	}

	reading, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("second Read() error: %v", err)
	}
	if reading.CPUUtilizationPct != 100 {
		t.Errorf("CPUUtilizationPct = %v, want 100", reading.CPUUtilizationPct)
	}
}

func TestParseCPUStatLine_Malformed(t *testing.T) {
	if _, err := parseCPUStatLine("not a cpu line\n"); err == nil {
		t.Error("parseCPUStatLine() succeeded on a malformed line, want an error")
	}
}

// TestCollector_Read_RealHardware exercises NewCollector's real nvidia-smi
// shell-out and real /proc parsing against whatever GPU/CPU this machine
// actually has - skipped (not failed) on a dev/CI environment with no GPU,
// same pattern as enginetransfer's TestRunCommand_RealTarBinary. Every
// other test in this file fakes commandRunner specifically because no GPU
// existed anywhere this project developed against until now (see
// PLANNING.md Known Issues) - this is the first test to confirm the
// documented nvidia-smi CSV shape this package assumes is actually what a
// real binary emits, not just well-documented behavior taken on faith.
// This also confirms the newer explicit index field parses correctly
// against this dev machine's real (single-GPU) nvidia-smi output - it does
// not confirm real multi-GPU line ordering, since no multi-GPU node exists
// here (PLANNING.md Known Issues).
func TestCollector_Read_RealHardware(t *testing.T) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		t.Skip("no nvidia-smi binary available")
	}

	c := NewCollector()

	first, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() error against real hardware: %v", err)
	}
	if len(first.GPUs) < 1 {
		t.Fatalf("len(GPUs) = %d, want >= 1", len(first.GPUs))
	}
	for _, g := range first.GPUs {
		if g.MemoryTotalMB <= 0 {
			t.Errorf("GPU %d MemoryTotalMB = %v, want > 0", g.Index, g.MemoryTotalMB)
		}
		if g.MemoryUsedMB < 0 || g.MemoryUsedMB > g.MemoryTotalMB {
			t.Errorf("GPU %d MemoryUsedMB = %v, want within [0, %v]", g.Index, g.MemoryUsedMB, g.MemoryTotalMB)
		}
		if g.UtilizationPct < 0 || g.UtilizationPct > 100 {
			t.Errorf("GPU %d UtilizationPct = %v, want within [0, 100]", g.Index, g.UtilizationPct)
		}
	}
	if first.SystemMemoryTotalMB <= 0 {
		t.Errorf("SystemMemoryTotalMB = %v, want > 0", first.SystemMemoryTotalMB)
	}
	if first.SystemMemoryUsedMB < 0 || first.SystemMemoryUsedMB > first.SystemMemoryTotalMB {
		t.Errorf("SystemMemoryUsedMB = %v, want within [0, %v]", first.SystemMemoryUsedMB, first.SystemMemoryTotalMB)
	}
	// First call has no prior /proc/stat sample - see Read's own doc comment.
	if first.CPUUtilizationPct != 0 {
		t.Errorf("CPUUtilizationPct on first real Read() = %v, want 0", first.CPUUtilizationPct)
	}

	time.Sleep(50 * time.Millisecond)

	second, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("second Read() error against real hardware: %v", err)
	}
	if second.CPUUtilizationPct < 0 || second.CPUUtilizationPct > 100 {
		t.Errorf("CPUUtilizationPct on second real Read() = %v, want within [0, 100]", second.CPUUtilizationPct)
	}
}

func TestCollector_Read_MissingMeminfoFields(t *testing.T) {
	procStat := "cpu  0 0 0 0 0 0 0 0 0 0\n"
	c := newTestCollector(t, fakeNvidiaSMI("0, 0, 0, 1", nil), procStat, "SomeOtherField: 123 kB\n")

	_, err := c.Read(context.Background())
	if err == nil {
		t.Fatal("Read() succeeded despite /proc/meminfo missing MemTotal/MemAvailable")
	}
}

// fakeNvidiaSMIRouter dispatches a different canned response depending on
// which nvidia-smi query is being run - needed once a single Read() can
// invoke nvidia-smi twice (the primary --query-gpu, and the
// --query-compute-apps fallback), unlike fakeNvidiaSMI's single fixed
// response.
func fakeNvidiaSMIRouter(queryGPU, computeApps string) commandRunner {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if strings.HasPrefix(a, "--query-compute-apps") {
				return []byte(computeApps), nil
			}
		}
		return []byte(queryGPU), nil
	}
}

// TestCollector_Read_UnifiedMemory_NAFallback reproduces the real DGX
// Spark shape confirmed in PLANNING.md's 2026-08-19 Decisions Log entry:
// nvidia-smi's aggregate memory.used/memory.total both come back "[N/A]"
// on the GB10's unified memory architecture, but utilization.gpu and
// per-process compute-apps memory are both real.
func TestCollector_Read_UnifiedMemory_NAFallback(t *testing.T) {
	procStat := "cpu  0 0 0 0 0 0 0 0 0 0\n"
	run := fakeNvidiaSMIRouter(
		"0, 96, [N/A], [N/A]",
		"30662, 133\n30822, 127\n2986705, 10726",
	)
	c := newTestCollector(t, run, procStat, sampleMeminfo)

	reading, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if len(reading.GPUs) != 1 {
		t.Fatalf("len(GPUs) = %d, want 1", len(reading.GPUs))
	}
	g := reading.GPUs[0]
	if g.UtilizationPct != 96 {
		t.Errorf("UtilizationPct = %v, want 96 (must not be lost to the memory fallback)", g.UtilizationPct)
	}
	wantUsed := float64(133 + 127 + 10726)
	if g.MemoryUsedMB != wantUsed {
		t.Errorf("MemoryUsedMB = %v, want %v (summed from compute-apps)", g.MemoryUsedMB, wantUsed)
	}
	wantTotal := float64(16186088) / 1024 // sampleMeminfo's MemTotal, in MB
	if g.MemoryTotalMB != wantTotal {
		t.Errorf("MemoryTotalMB = %v, want %v (system MemTotal)", g.MemoryTotalMB, wantTotal)
	}
}

// TestCollector_Read_UnifiedMemory_OnlyTotalNA covers the total-only half
// of the fallback independently of the used-memory half, in case a future
// driver reports them differently.
func TestCollector_Read_UnifiedMemory_OnlyTotalNA(t *testing.T) {
	procStat := "cpu  0 0 0 0 0 0 0 0 0 0\n"
	c := newTestCollector(t, fakeNvidiaSMI("0, 50, 4096, [N/A]", nil), procStat, sampleMeminfo)

	reading, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	g := reading.GPUs[0]
	if g.MemoryUsedMB != 4096 {
		t.Errorf("MemoryUsedMB = %v, want 4096 (real value, no fallback needed)", g.MemoryUsedMB)
	}
	wantTotal := float64(16186088) / 1024
	if g.MemoryTotalMB != wantTotal {
		t.Errorf("MemoryTotalMB = %v, want %v (system MemTotal)", g.MemoryTotalMB, wantTotal)
	}
}

// TestCollector_Read_UnifiedMemory_ComputeAppsQueryFails confirms a
// failure in the fallback query itself degrades to 0 rather than failing
// the whole reading - this is already the degraded path, so utilization/
// CPU/system-memory telemetry must still get through.
func TestCollector_Read_UnifiedMemory_ComputeAppsQueryFails(t *testing.T) {
	procStat := "cpu  0 0 0 0 0 0 0 0 0 0\n"
	callCount := 0
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		callCount++
		for _, a := range args {
			if strings.HasPrefix(a, "--query-compute-apps") {
				return nil, errors.New("nvidia-smi: compute-apps query failed")
			}
		}
		return []byte("0, 96, [N/A], [N/A]"), nil
	}
	c := newTestCollector(t, run, procStat, sampleMeminfo)

	reading, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("nvidia-smi called %d times, want 2 (primary query + fallback)", callCount)
	}
	if reading.GPUs[0].MemoryUsedMB != 0 {
		t.Errorf("MemoryUsedMB = %v, want 0 (fallback query failed)", reading.GPUs[0].MemoryUsedMB)
	}
	if reading.GPUs[0].UtilizationPct != 96 {
		t.Errorf("UtilizationPct = %v, want 96 (must still get through)", reading.GPUs[0].UtilizationPct)
	}
}

// TestCollector_Read_UnifiedMemory_SkipsUnparseableComputeAppsLines
// confirms one bad line in the compute-apps fallback (an N/A used_memory,
// or a malformed line) doesn't zero out or fail the whole sum - see
// sumComputeAppsMemory's own doc comment.
func TestCollector_Read_UnifiedMemory_SkipsUnparseableComputeAppsLines(t *testing.T) {
	procStat := "cpu  0 0 0 0 0 0 0 0 0 0\n"
	run := fakeNvidiaSMIRouter(
		"0, 10, [N/A], [N/A]",
		"111, 500\n222, [N/A]\nnot,a,valid,line\n333, 250",
	)
	c := newTestCollector(t, run, procStat, sampleMeminfo)

	reading, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if reading.GPUs[0].MemoryUsedMB != 750 {
		t.Errorf("MemoryUsedMB = %v, want 750 (500+250, skipping the N/A and malformed lines)", reading.GPUs[0].MemoryUsedMB)
	}
}
