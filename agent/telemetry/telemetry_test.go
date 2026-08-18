// SPDX-License-Identifier: AGPL-3.0-or-later

package telemetry

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	c := newTestCollector(t, fakeNvidiaSMI("45, 8192, 24576", nil), procStat, sampleMeminfo)

	reading, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if reading.GPUUtilizationPct != 45 {
		t.Errorf("GPUUtilizationPct = %v, want 45", reading.GPUUtilizationPct)
	}
	if reading.GPUMemoryUsedMB != 8192 || reading.GPUMemoryTotalMB != 24576 {
		t.Errorf("GPUMemoryUsedMB/TotalMB = %v/%v, want 8192/24576", reading.GPUMemoryUsedMB, reading.GPUMemoryTotalMB)
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

func TestCollector_Read_MultipleGPUs_Aggregated(t *testing.T) {
	procStat := "cpu  0 0 0 0 0 0 0 0 0 0\n"
	// Two GPUs: utilization averaged (40, 60 -> 50), memory summed.
	c := newTestCollector(t, fakeNvidiaSMI("40, 4096, 24576\n60, 6144, 24576", nil), procStat, sampleMeminfo)

	reading, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if reading.GPUUtilizationPct != 50 {
		t.Errorf("GPUUtilizationPct = %v, want 50 (average of 40 and 60)", reading.GPUUtilizationPct)
	}
	if reading.GPUMemoryUsedMB != 10240 {
		t.Errorf("GPUMemoryUsedMB = %v, want 10240 (sum of 4096 and 6144)", reading.GPUMemoryUsedMB)
	}
	if reading.GPUMemoryTotalMB != 49152 {
		t.Errorf("GPUMemoryTotalMB = %v, want 49152 (sum of 24576 and 24576)", reading.GPUMemoryTotalMB)
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
	c := newTestCollector(t, fakeNvidiaSMI("0, 0, 1", nil), "", sampleMeminfo)
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
func TestCollector_Read_RealHardware(t *testing.T) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		t.Skip("no nvidia-smi binary available")
	}

	c := NewCollector()

	first, err := c.Read(context.Background())
	if err != nil {
		t.Fatalf("Read() error against real hardware: %v", err)
	}
	if first.GPUMemoryTotalMB <= 0 {
		t.Errorf("GPUMemoryTotalMB = %v, want > 0", first.GPUMemoryTotalMB)
	}
	if first.GPUMemoryUsedMB < 0 || first.GPUMemoryUsedMB > first.GPUMemoryTotalMB {
		t.Errorf("GPUMemoryUsedMB = %v, want within [0, %v]", first.GPUMemoryUsedMB, first.GPUMemoryTotalMB)
	}
	if first.GPUUtilizationPct < 0 || first.GPUUtilizationPct > 100 {
		t.Errorf("GPUUtilizationPct = %v, want within [0, 100]", first.GPUUtilizationPct)
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
	c := newTestCollector(t, fakeNvidiaSMI("0, 0, 1", nil), procStat, "SomeOtherField: 123 kB\n")

	_, err := c.Read(context.Background())
	if err == nil {
		t.Fatal("Read() succeeded despite /proc/meminfo missing MemTotal/MemAvailable")
	}
}
