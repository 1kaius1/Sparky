// SPDX-License-Identifier: AGPL-3.0-or-later

// Package telemetry is sparky-agent's Telemetry Collector - see
// ARCHITECTURE.md's Telemetry Collector component and docs/AGENT.md
// Service Architecture Notes' Telemetry goroutine. Reads nvidia-smi and
// /proc directly, the same technique already proven by existing
// Spark-hardware Prometheus exporters (ARCHITECTURE.md), rather than
// depending on a metrics library or a running Prometheus exporter.
package telemetry

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// GPUReading is a single point-in-time reading from one physical GPU - see
// SCHEMA.md GPU metrics. Index comes from nvidia-smi's own reported index
// field, not assumed line order - see readGPU.
type GPUReading struct {
	Index          int
	UtilizationPct float64
	MemoryUsedMB   float64
	MemoryTotalMB  float64
}

// Reading is a single point-in-time hardware snapshot - see SCHEMA.md
// Metrics and GPU metrics, whose columns this mirrors exactly. CPU and
// system memory stay node-level regardless of GPU count (one CPU, one RAM
// pool per node), while GPUs is one entry per physical GPU nvidia-smi
// reports - see readGPU.
type Reading struct {
	GPUs                []GPUReading
	CPUUtilizationPct   float64
	SystemMemoryUsedMB  float64
	SystemMemoryTotalMB float64
}

// commandRunner abstracts running an external command, narrow enough to
// fake in tests without actually invoking nvidia-smi - no GPU exists in
// this project's own dev environment (PLANNING.md Dependencies and
// Blockers), so this boundary is what makes GPU parsing testable at all;
// the real nvidia-smi CSV shape is documented, well-established behavior,
// not independently verified here.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// cpuSample is one /proc/stat aggregate-line reading, kept between Read
// calls to compute utilization from the delta - see readCPU.
type cpuSample struct {
	idle  uint64
	total uint64
}

// Collector reads hardware telemetry from the local node. Not safe for
// concurrent use - agent/connection's telemetry goroutine is the only
// caller, and CPU utilization needs state carried between successive
// calls (see readCPU), the same shape as a stateful rate counter.
type Collector struct {
	runCommand      commandRunner
	procStatPath    string
	procMeminfoPath string

	prevCPU *cpuSample
}

// NewCollector constructs a Collector that reads real nvidia-smi output
// and the real /proc.
func NewCollector() *Collector {
	return &Collector{
		runCommand:      runCommand,
		procStatPath:    "/proc/stat",
		procMeminfoPath: "/proc/meminfo",
	}
}

// Read takes one hardware snapshot. CPUUtilizationPct is 0 on the very
// first call from a fresh Collector - CPU utilization is inherently a
// rate, not an instantaneous value (/proc/stat exposes cumulative
// counters since boot), so a first reading has no prior sample to compute
// a delta against; every subsequent call reports a real value.
func (c *Collector) Read(ctx context.Context) (Reading, error) {
	memUsedMB, memTotalMB, err := c.readMemory()
	if err != nil {
		return Reading{}, fmt.Errorf("read memory telemetry: %w", err)
	}
	gpus, err := c.readGPU(ctx, memTotalMB)
	if err != nil {
		return Reading{}, fmt.Errorf("read GPU telemetry: %w", err)
	}
	cpuUtil, err := c.readCPU()
	if err != nil {
		return Reading{}, fmt.Errorf("read CPU telemetry: %w", err)
	}

	return Reading{
		GPUs:                gpus,
		CPUUtilizationPct:   cpuUtil,
		SystemMemoryUsedMB:  memUsedMB,
		SystemMemoryTotalMB: memTotalMB,
	}, nil
}

// readGPU shells out to nvidia-smi and reports one GPUReading per physical
// GPU it lists - nvidia-smi is NVIDIA-proprietary tooling scoped to NVIDIA
// hardware only, which is the correct scope here: Sparky's engine adapters
// (vLLM, Aphrodite, llama.cpp) are CUDA-first, so a non-NVIDIA GPU on a node
// (e.g. an integrated GPU that's part of the CPU) is never something Sparky
// could schedule an inference engine onto, and deliberately never appears
// in this reading. The query requests an explicit index field so GPU
// identity comes from nvidia-smi itself rather than assumed line order.
// This has been verified against real single-GPU nvidia-smi output
// (PLANNING.md Decisions Log) but never against a real multi-GPU node -
// every node available to this project (the laptop RTX 4090, the Dell
// Precision RTX 3080Ti) has exactly one GPU, so whether a real multi-GPU
// nvidia-smi invocation emits one CSV line per GPU in the order assumed
// here remains an honest, documented gap (PLANNING.md Known Issues).
//
// memory.used/memory.total can each come back as nvidia-smi's own literal
// "[N/A]" instead of a number - confirmed on a real DGX Spark's GB10,
// whose nvidia-smi table view reports GPU memory as flatly "Not
// Supported", almost certainly a consequence of Grace-Blackwell's unified
// CPU/GPU memory architecture rather than a transient or query-syntax
// issue (PLANNING.md's 2026-08-19 Decisions Log entry). systemMemTotalMB
// (the caller's own already-read /proc/meminfo MemTotal) stands in for an
// N/A memory.total - SCHEMA.md's Nodes documentation already establishes
// that a Spark's GPU capacity *is* system memory capacity, not an
// approximation. An N/A memory.used falls back to summing real
// per-process usage instead - see sumComputeAppsMemory.
func (c *Collector) readGPU(ctx context.Context, systemMemTotalMB float64) ([]GPUReading, error) {
	out, err := c.runCommand(ctx, "nvidia-smi", "--query-gpu=index,utilization.gpu,memory.used,memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("run nvidia-smi: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil, fmt.Errorf("nvidia-smi reported no GPUs")
	}

	gpus := make([]GPUReading, 0, len(lines))
	var needsUsedFallback []int
	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) != 4 {
			return nil, fmt.Errorf("unexpected nvidia-smi output line %q", line)
		}
		index, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			return nil, fmt.Errorf("parse index from %q: %w", line, err)
		}
		util, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("parse utilization.gpu from %q: %w", line, err)
		}

		var used float64
		usedField := strings.TrimSpace(fields[2])
		if usedField == naMarker {
			needsUsedFallback = append(needsUsedFallback, len(gpus))
		} else if used, err = strconv.ParseFloat(usedField, 64); err != nil {
			return nil, fmt.Errorf("parse memory.used from %q: %w", line, err)
		}

		var total float64
		totalField := strings.TrimSpace(fields[3])
		if totalField == naMarker {
			total = systemMemTotalMB
		} else if total, err = strconv.ParseFloat(totalField, 64); err != nil {
			return nil, fmt.Errorf("parse memory.total from %q: %w", line, err)
		}

		gpus = append(gpus, GPUReading{Index: index, UtilizationPct: util, MemoryUsedMB: used, MemoryTotalMB: total})
	}

	if len(needsUsedFallback) > 0 {
		// Applied to every GPU that reported N/A: this query has no
		// per-GPU attribution (no gpu_uuid correlation) to split the
		// sum by when more than one GPU needs it - an honest gap left
		// unresolved same as this function's other multi-GPU caveats,
		// since every node available to this project, Spark included,
		// has exactly one GPU to verify against.
		usedSum := c.sumComputeAppsMemory(ctx)
		for _, i := range needsUsedFallback {
			gpus[i].MemoryUsedMB = usedSum
		}
	}

	return gpus, nil
}

// naMarker is nvidia-smi's own literal string for a query field it can't
// answer - see readGPU.
const naMarker = "[N/A]"

// sumComputeAppsMemory sums real per-process GPU memory usage, for use
// only when the aggregate memory.used query itself is unsupported (see
// readGPU) - confirmed queryable on the same DGX Spark whose aggregate
// query is not, via nvidia-smi's own process table. A query failure or a
// line nvidia-smi itself can't attribute (a used_memory of "[N/A]", seen
// for some non-compute entries) is skipped rather than failing the whole
// reading - this is already the degraded fallback path, so a partial sum
// is strictly better than losing GPU memory telemetry entirely over a
// secondary measurement's own failure.
func (c *Collector) sumComputeAppsMemory(ctx context.Context) float64 {
	out, err := c.runCommand(ctx, "nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
	if err != nil {
		return 0
	}

	var sum float64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			continue
		}
		used := strings.TrimSpace(fields[1])
		if used == naMarker {
			continue
		}
		v, err := strconv.ParseFloat(used, 64)
		if err != nil {
			continue
		}
		sum += v
	}
	return sum
}

// readCPU parses /proc/stat's aggregate "cpu " line and computes
// utilization from the delta against the previous call - guest/guest_nice
// are excluded from the total since the kernel already counts them within
// user/nice (Linux's own Documentation/filesystems/proc.rst), so including
// them again would overstate total ticks; the same technique
// top/htop-style CPU percent calculations use.
func (c *Collector) readCPU() (float64, error) {
	data, err := os.ReadFile(c.procStatPath)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", c.procStatPath, err)
	}

	sample, err := parseCPUStatLine(string(data))
	if err != nil {
		return 0, err
	}

	prev := c.prevCPU
	c.prevCPU = &sample
	if prev == nil {
		return 0, nil
	}

	totalDelta := sample.total - prev.total
	idleDelta := sample.idle - prev.idle
	if totalDelta == 0 {
		return 0, nil
	}
	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100, nil
}

// parseCPUStatLine parses /proc/stat's first line - "cpu  user nice
// system idle iowait irq softirq steal guest guest_nice" - confirmed
// against a real /proc/stat on this dev machine, not assumed.
func parseCPUStatLine(procStat string) (cpuSample, error) {
	firstLine, _, _ := strings.Cut(procStat, "\n")
	fields := strings.Fields(firstLine)
	if len(fields) < 9 || fields[0] != "cpu" {
		return cpuSample{}, fmt.Errorf("unexpected /proc/stat first line %q", firstLine)
	}

	// user, nice, system, idle, iowait, irq, softirq, steal.
	values := make([]uint64, 8)
	for i := range values {
		v, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return cpuSample{}, fmt.Errorf("parse /proc/stat field %d (%q): %w", i+1, fields[i+1], err)
		}
		values[i] = v
	}

	idle := values[3] + values[4] // idle + iowait
	var total uint64
	for _, v := range values {
		total += v
	}
	return cpuSample{idle: idle, total: total}, nil
}

// readMemory parses /proc/meminfo's MemTotal and MemAvailable (kB) -
// MemAvailable, not MemFree, since it accounts for reclaimable
// cache/buffers, the modern correct notion of "how much could a new
// process actually use" per the kernel's own
// Documentation/filesystems/proc.rst.
func (c *Collector) readMemory() (usedMB, totalMB float64, err error) {
	data, err := os.ReadFile(c.procMeminfoPath)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", c.procMeminfoPath, err)
	}

	var totalKB, availableKB uint64
	var haveTotal, haveAvailable bool
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			totalKB, err = strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse MemTotal from %q: %w", line, err)
			}
			haveTotal = true
		case "MemAvailable":
			availableKB, err = strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse MemAvailable from %q: %w", line, err)
			}
			haveAvailable = true
		}
	}
	if !haveTotal || !haveAvailable {
		return 0, 0, fmt.Errorf("MemTotal/MemAvailable not found in %s", c.procMeminfoPath)
	}

	totalMB = float64(totalKB) / 1024
	usedMB = float64(totalKB-availableKB) / 1024
	return usedMB, totalMB, nil
}
