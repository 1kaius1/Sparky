// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build ignore

// fleetbench is the concurrent HTTP/SSE client, timing, and percentile-math
// half of the scripts/dev-test.sh fleet benchmark harness - see
// PLAN-FLEET_PROMPT_TEST.md. It never runs as part of `go build ./...` or
// `go vet ./...` (the ignore build tag above keeps it out of the module's
// normal build/test surface) and is only ever invoked via `go run
// scripts/fleetbench.go ...` from dev-test.sh.
//
// Two modes, selected by -mode:
//
//   - "run" (default): fire one batch of C concurrent OpenAI-compatible
//     chat-completion requests against every target in a target file, all
//     starting together, and print one BatchResult as JSON. dev-test.sh
//     calls this once per (test, concurrency level, repeat) and redirects
//     stdout to a file under its results directory.
//   - "aggregate": read every batch_*.json (and matching samples_*.csv GPU
//     sample file, if present) under -results-dir, group by (test, target,
//     concurrency) across repeats, compute the metrics PLAN-FLEET_PROMPT_TEST.md
//     section 3 defines, and print the human-readable report plus write
//     summary.csv/run.json/sample text under -out-dir.
//
// dev-test.sh owns everything this file does not: argument/menu handling,
// reading the target file for its own purposes (ssh_host), warmup requests,
// the pre-flight model-mismatch check, and GPU/vLLM-metrics sampling via
// SSH (written to the samples_*.csv files this file's aggregate mode reads).
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------
// Shared types (the JSON contract between "run" mode's output and
// "aggregate" mode's input).
// ---------------------------------------------------------------------

// target is one line of the dev-test.sh target file - see
// dev-test.targets.example. ssh_host is carried through for completeness
// but unused here; GPU sampling is dev-test.sh's job.
type target struct {
	Name    string
	BaseURL string
	SSHHost string
}

// requestResult is one client-measured chat-completion request - see
// PLAN-FLEET_PROMPT_TEST.md section 3's metric definitions table.
type requestResult struct {
	Index            int     `json:"index"`
	TTFTMs           float64 `json:"ttft_ms,omitempty"`
	TotalMs          float64 `json:"total_ms"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	Error            string  `json:"error,omitempty"`
	SampleText       string  `json:"sample_text,omitempty"`
}

// targetBatchResult is one target's slice of a batch.
type targetBatchResult struct {
	Name       string          `json:"name"`
	BaseURL    string          `json:"base_url"`
	Model      string          `json:"model"`
	ModelErr   string          `json:"model_error,omitempty"`
	Requests   []requestResult `json:"requests"`
	ErrorCount int             `json:"error_count"`
}

// batchResult is what "run" mode emits - one (test, concurrency, repeat)
// batch across every target.
type batchResult struct {
	Test        string              `json:"test"`
	Concurrency int                 `json:"concurrency"`
	Repeat      int                 `json:"repeat"`
	PromptFile  string              `json:"prompt_file"`
	MaxTokens   int                 `json:"max_tokens"`
	IgnoreEOS   bool                `json:"ignore_eos"`
	StartedAt   time.Time           `json:"started_at"`
	BatchWallS  float64             `json:"batch_wall_s"`
	Targets     []targetBatchResult `json:"targets"`
}

func main() {
	mode := flag.String("mode", "run", "run | aggregate")
	// Shared/run flags.
	targetsPath := flag.String("targets", "", "path to the target file (run mode)")
	testName := flag.String("test", "", "test/scenario label, e.g. decode or prefill (run mode)")
	concurrency := flag.Int("concurrency", 1, "number of concurrent requests per target (run mode)")
	repeat := flag.Int("repeat", 1, "repeat index, for labeling only (run mode)")
	promptFile := flag.String("prompt-file", "", "path to the prompt text to send (run mode)")
	maxTokens := flag.Int("max-tokens", 2000, "max_tokens / forced generation length (run mode)")
	temperature := flag.Float64("temperature", 0, "sampling temperature (run mode)")
	seed := flag.Int("seed", 0, "sampling seed (run mode)")
	ignoreEOS := flag.Bool("ignore-eos", true, "set vLLM's ignore_eos so every request generates exactly max-tokens (run mode)")
	timeout := flag.Duration("timeout", 5*time.Minute, "per-request timeout (run mode)")
	out := flag.String("out", "", "also write the batch JSON to this file (run mode)")

	// Aggregate flags.
	resultsDir := flag.String("results-dir", "", "directory of batch_*.json / samples_*.csv files (aggregate mode)")
	outDir := flag.String("out-dir", "", "directory to write summary.csv/run.json/report into (aggregate mode)")
	saturation := flag.String("saturation", "aggressive", "aggressive | lenient - which QoS threshold headlines the recommendation (aggregate mode)")
	expectedDailyRequests := flag.Int("expected-daily-requests", 0, "optional headroom readout input (aggregate mode)")
	typicalOutputTokens := flag.Int("typical-output-tokens", 0, "optional headroom readout input (aggregate mode)")

	flag.Parse()

	var err error
	switch *mode {
	case "run":
		err = runMode(runOpts{
			targetsPath: *targetsPath,
			test:        *testName,
			concurrency: *concurrency,
			repeat:      *repeat,
			promptFile:  *promptFile,
			maxTokens:   *maxTokens,
			temperature: *temperature,
			seed:        *seed,
			ignoreEOS:   *ignoreEOS,
			timeout:     *timeout,
			out:         *out,
		})
	case "aggregate":
		err = aggregateMode(aggregateOpts{
			resultsDir:            *resultsDir,
			outDir:                *outDir,
			saturation:            *saturation,
			expectedDailyRequests: *expectedDailyRequests,
			typicalOutputTokens:   *typicalOutputTokens,
		})
	default:
		err = fmt.Errorf("unknown -mode %q (want run or aggregate)", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetbench:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------
// Target file parsing (shared).
// ---------------------------------------------------------------------

func loadTargets(path string) ([]target, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open target file: %w", err)
	}
	defer f.Close()

	var targets []target
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed target line %q: want at least name and base_url", line)
		}
		t := target{Name: fields[0], BaseURL: strings.TrimRight(fields[1], "/")}
		if len(fields) >= 3 {
			t.SSHHost = fields[2]
		}
		targets = append(targets, t)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read target file: %w", err)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no targets found in %s", path)
	}
	return targets, nil
}

// ---------------------------------------------------------------------
// Run mode.
// ---------------------------------------------------------------------

type runOpts struct {
	targetsPath string
	test        string
	concurrency int
	repeat      int
	promptFile  string
	maxTokens   int
	temperature float64
	seed        int
	ignoreEOS   bool
	timeout     time.Duration
	out         string
}

func runMode(o runOpts) error {
	if o.targetsPath == "" || o.test == "" || o.promptFile == "" {
		return fmt.Errorf("run mode requires -targets, -test, and -prompt-file")
	}
	targets, err := loadTargets(o.targetsPath)
	if err != nil {
		return err
	}
	promptBytes, err := os.ReadFile(o.promptFile)
	if err != nil {
		return fmt.Errorf("read prompt file: %w", err)
	}
	prompt := strings.TrimSpace(string(promptBytes))

	client := &http.Client{}

	// Resolve each target's live model id up front - see the readiness-probe
	// lesson recorded in PLANNING.md 2026-09-11: a server-side
	// served_model_name override means the id a request must use can only be
	// known by asking the server, never assumed from the profile/model_ref.
	targetModels := make([]string, len(targets))
	for i, t := range targets {
		id, err := resolveModelID(client, t.BaseURL)
		if err != nil {
			targetModels[i] = ""
		} else {
			targetModels[i] = id
		}
	}

	result := batchResult{
		Test:        o.test,
		Concurrency: o.concurrency,
		Repeat:      o.repeat,
		PromptFile:  o.promptFile,
		MaxTokens:   o.maxTokens,
		IgnoreEOS:   o.ignoreEOS,
		Targets:     make([]targetBatchResult, len(targets)),
	}

	// A start barrier so every goroutine, across every target, begins its
	// request at the same instant - batch_wall_s is measured from the
	// barrier release, not from goroutine-launch order.
	var wg sync.WaitGroup
	start := make(chan struct{})

	for ti, t := range targets {
		tbr := &targetBatchResult{Name: t.Name, BaseURL: t.BaseURL, Model: targetModels[ti]}
		if targetModels[ti] == "" {
			tbr.ModelErr = "could not resolve model id from GET " + t.BaseURL + "/v1/models"
		}
		tbr.Requests = make([]requestResult, o.concurrency)
		result.Targets[ti] = *tbr
		target := t
		targetIdx := ti
		modelID := targetModels[ti]

		for i := 0; i < o.concurrency; i++ {
			wg.Add(1)
			reqIndex := i
			go func() {
				defer wg.Done()
				<-start
				res := doChatCompletion(client, chatCompletionParams{
					baseURL:     target.BaseURL,
					model:       modelID,
					prompt:      prompt,
					maxTokens:   o.maxTokens,
					temperature: o.temperature,
					seed:        o.seed,
					ignoreEOS:   o.ignoreEOS,
					timeout:     o.timeout,
					captureText: reqIndex == 0,
				})
				res.Index = reqIndex
				result.Targets[targetIdx].Requests[reqIndex] = res
				if res.Error != "" {
					result.Targets[targetIdx].ErrorCount++
				}
			}()
		}
	}

	result.StartedAt = time.Now()
	batchStart := time.Now()
	close(start)
	wg.Wait()
	result.BatchWallS = time.Since(batchStart).Seconds()

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if o.out != "" {
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result for -out: %w", err)
		}
		if err := os.WriteFile(o.out, data, 0o644); err != nil {
			return fmt.Errorf("write -out file: %w", err)
		}
	}
	return nil
}

func resolveModelID(client *http.Client, baseURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /v1/models: status %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if len(parsed.Data) == 0 {
		return "", fmt.Errorf("GET /v1/models: empty data array")
	}
	return parsed.Data[0].ID, nil
}

type chatCompletionParams struct {
	baseURL     string
	model       string
	prompt      string
	maxTokens   int
	temperature float64
	seed        int
	ignoreEOS   bool
	timeout     time.Duration
	captureText bool
}

// sseChunk is the subset of an OpenAI-compatible chat-completion stream
// chunk this tool reads: the first non-empty delta.content marks TTFT, and
// a populated usage object (sent as its own trailing chunk when
// stream_options.include_usage is set) carries the authoritative token
// counts - see PLAN-FLEET_PROMPT_TEST.md section 3.
type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func doChatCompletion(client *http.Client, p chatCompletionParams) requestResult {
	if p.model == "" {
		return requestResult{Error: "no model id resolved for this target"}
	}

	body := map[string]any{
		"model":       p.model,
		"messages":    []map[string]string{{"role": "user", "content": p.prompt}},
		"max_tokens":  p.maxTokens,
		"temperature": p.temperature,
		"seed":        p.seed,
		"stream":      true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	if p.ignoreEOS {
		body["ignore_eos"] = true
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return requestResult{Error: "marshal request: " + err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", strings.NewReader(string(payload)))
	if err != nil {
		return requestResult{Error: "build request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	sendTime := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return requestResult{Error: "request failed: " + err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		sample, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return requestResult{Error: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(sample)))}
	}

	res := requestResult{}
	var ttft time.Duration
	gotFirstToken := false
	var textBuilder strings.Builder

	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				break
			}
			if payload != "" {
				var chunk sseChunk
				if jsonErr := json.Unmarshal([]byte(payload), &chunk); jsonErr == nil {
					if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
						if !gotFirstToken {
							ttft = time.Since(sendTime)
							gotFirstToken = true
						}
						if p.captureText {
							textBuilder.WriteString(chunk.Choices[0].Delta.Content)
						}
					}
					if chunk.Usage != nil {
						res.PromptTokens = chunk.Usage.PromptTokens
						res.CompletionTokens = chunk.Usage.CompletionTokens
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			res.Error = "stream read error: " + err.Error()
			break
		}
	}

	res.TotalMs = float64(time.Since(sendTime).Microseconds()) / 1000
	if gotFirstToken {
		res.TTFTMs = float64(ttft.Microseconds()) / 1000
	} else if res.Error == "" {
		res.Error = "stream ended with no content tokens"
	}
	if p.captureText {
		text := textBuilder.String()
		if len(text) > 800 {
			text = text[:800] + "..."
		}
		res.SampleText = text
	}
	return res
}

// ---------------------------------------------------------------------
// Aggregate mode.
// ---------------------------------------------------------------------

type aggregateOpts struct {
	resultsDir            string
	outDir                string
	saturation            string
	expectedDailyRequests int
	typicalOutputTokens   int
}

// sampleRow is one tick of a samples_<test>_<concurrency>_<repeat>_<target>.csv
// file written by dev-test.sh's SSH-based GPU/metrics sampler - see that
// script for the exact columns.
type sampleRow struct {
	EpochMs             int64
	UtilPct             *float64
	MemUsedMB           *float64
	PowerW              *float64
	NumRequestsRunning  *float64
	NumRequestsWaiting  *float64
	GPUCacheUsagePerc   *float64
	PromptTokensTotal   *float64
	GenerationTokensTot *float64
}

// repeatRecord is one batch's contribution to a (test, target, concurrency)
// group - kept separate per repeat so throughput (which depends on that
// specific batch's wall time) is computed per repeat before being combined
// statistically across repeats, per PLAN-FLEET_PROMPT_TEST.md D5.
type repeatRecord struct {
	wallS    float64
	requests []requestResult
	samples  []sampleRow
	model    string
}

type groupKey struct {
	test        string
	name        string
	concurrency int
}

func aggregateMode(o aggregateOpts) error {
	if o.resultsDir == "" || o.outDir == "" {
		return fmt.Errorf("aggregate mode requires -results-dir and -out-dir")
	}
	if o.saturation != "aggressive" && o.saturation != "lenient" {
		return fmt.Errorf("-saturation must be aggressive or lenient, got %q", o.saturation)
	}
	if err := os.MkdirAll(o.outDir, 0o755); err != nil {
		return fmt.Errorf("create out-dir: %w", err)
	}

	batchFiles, err := filepath.Glob(filepath.Join(o.resultsDir, "batch_*.json"))
	if err != nil {
		return fmt.Errorf("glob batch files: %w", err)
	}
	if len(batchFiles) == 0 {
		return fmt.Errorf("no batch_*.json files found in %s", o.resultsDir)
	}
	sort.Strings(batchFiles)

	groups := map[groupKey][]repeatRecord{}
	var allBatches []batchResult
	var groupOrder []groupKey

	for _, bf := range batchFiles {
		data, err := os.ReadFile(bf)
		if err != nil {
			return fmt.Errorf("read %s: %w", bf, err)
		}
		var br batchResult
		if err := json.Unmarshal(data, &br); err != nil {
			return fmt.Errorf("parse %s: %w", bf, err)
		}
		allBatches = append(allBatches, br)

		for _, tbr := range br.Targets {
			key := groupKey{test: br.Test, name: tbr.Name, concurrency: br.Concurrency}
			if _, seen := groups[key]; !seen {
				groupOrder = append(groupOrder, key)
			}
			samplesPath := filepath.Join(o.resultsDir, fmt.Sprintf("samples_%s_%d_%d_%s.csv", br.Test, br.Concurrency, br.Repeat, tbr.Name))
			samples, _ := loadSamples(samplesPath) // best-effort; nil if absent
			groups[key] = append(groups[key], repeatRecord{
				wallS:    br.BatchWallS,
				requests: tbr.Requests,
				samples:  samples,
				model:    tbr.Model,
			})
		}
	}

	sort.Slice(groupOrder, func(i, j int) bool {
		a, b := groupOrder[i], groupOrder[j]
		if a.test != b.test {
			return a.test < b.test
		}
		if a.name != b.name {
			return a.name < b.name
		}
		return a.concurrency < b.concurrency
	})

	stats := make(map[groupKey]groupStats, len(groupOrder))
	for _, k := range groupOrder {
		stats[k] = computeGroupStats(groups[k])
	}

	if err := writeSummaryCSV(filepath.Join(o.outDir, "summary.csv"), groupOrder, stats); err != nil {
		return err
	}
	if err := writeRunJSON(filepath.Join(o.outDir, "run.json"), allBatches); err != nil {
		return err
	}
	writeSampleTexts(o.outDir, groupOrder, groups)

	printReport(groupOrder, stats, o.saturation, o.expectedDailyRequests, o.typicalOutputTokens)
	fmt.Printf("\nArtifacts written to %s\n", o.outDir)
	return nil
}

func loadSamples(path string) ([]sampleRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	rows, err := r.ReadAll()
	if err != nil || len(rows) < 2 {
		return nil, err
	}
	var out []sampleRow
	for _, row := range rows[1:] { // skip header
		if len(row) < 9 {
			continue
		}
		epoch, _ := strconv.ParseInt(row[0], 10, 64)
		out = append(out, sampleRow{
			EpochMs:             epoch,
			UtilPct:             parseOptFloat(row[1]),
			MemUsedMB:           parseOptFloat(row[2]),
			PowerW:              parseOptFloat(row[3]),
			NumRequestsRunning:  parseOptFloat(row[4]),
			NumRequestsWaiting:  parseOptFloat(row[5]),
			GPUCacheUsagePerc:   parseOptFloat(row[6]),
			PromptTokensTotal:   parseOptFloat(row[7]),
			GenerationTokensTot: parseOptFloat(row[8]),
		})
	}
	return out, nil
}

func parseOptFloat(s string) *float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &f
}

// ---------------------------------------------------------------------
// Statistics.
// ---------------------------------------------------------------------

type groupStats struct {
	model           string
	requestCount    int
	errorCount      int
	errorSamples    []string
	ttftP50         float64
	ttftP90         float64
	ttftP99         float64
	ttftMax         float64
	decodeTokSP50   float64
	decodeTokSP90   float64
	itlP50          float64
	itlP90          float64
	aggTokSMean     float64
	aggTokSStdev    float64
	gpuUtilMean     *float64
	gpuUtilPeak     *float64
	gpuMemMeanMB    *float64
	gpuMemPeakMB    *float64
	gpuPowerMeanW   *float64
	gpuPowerPeakW   *float64
	waitingEverPeak *float64
	runningPeak     *float64
	cacheUsagePeak  *float64
	promptTokDelta  *float64
	genTokDelta     *float64
	repeats         int
}

func computeGroupStats(records []repeatRecord) groupStats {
	var s groupStats
	s.repeats = len(records)
	if len(records) > 0 {
		s.model = records[0].model
	}

	var ttfts, decodeToks, itls []float64
	var aggTokS []float64

	for _, rec := range records {
		var completionSum int
		for _, req := range rec.requests {
			s.requestCount++
			if req.Error != "" {
				s.errorCount++
				if len(s.errorSamples) < 3 {
					s.errorSamples = append(s.errorSamples, req.Error)
				}
				continue
			}
			if req.TTFTMs > 0 {
				ttfts = append(ttfts, req.TTFTMs)
			}
			completionSum += req.CompletionTokens
			decodeS := (req.TotalMs - req.TTFTMs) / 1000
			if decodeS > 0 && req.CompletionTokens > 0 {
				decodeToks = append(decodeToks, float64(req.CompletionTokens)/decodeS)
			}
			if req.CompletionTokens > 1 && req.TotalMs > req.TTFTMs {
				itl := (req.TotalMs - req.TTFTMs) / float64(req.CompletionTokens-1)
				itls = append(itls, itl)
			}
		}
		if rec.wallS > 0 {
			aggTokS = append(aggTokS, float64(completionSum)/rec.wallS)
		}

		// Fold in this repeat's GPU/vLLM-metrics sample series.
		foldSamples(&s, rec.samples)
	}

	s.ttftP50 = percentile(ttfts, 50)
	s.ttftP90 = percentile(ttfts, 90)
	s.ttftP99 = percentile(ttfts, 99)
	s.ttftMax = maxOf(ttfts)
	s.decodeTokSP50 = percentile(decodeToks, 50)
	s.decodeTokSP90 = percentile(decodeToks, 90)
	s.itlP50 = percentile(itls, 50)
	s.itlP90 = percentile(itls, 90)
	s.aggTokSMean = mean(aggTokS)
	s.aggTokSStdev = stdev(aggTokS)
	return s
}

func foldSamples(s *groupStats, samples []sampleRow) {
	var utils, mems, powers, waitings, runnings, caches []float64
	var firstPrompt, lastPrompt, firstGen, lastGen *float64
	for _, row := range samples {
		if row.UtilPct != nil {
			utils = append(utils, *row.UtilPct)
		}
		if row.MemUsedMB != nil {
			mems = append(mems, *row.MemUsedMB)
		}
		if row.PowerW != nil {
			powers = append(powers, *row.PowerW)
		}
		if row.NumRequestsWaiting != nil {
			waitings = append(waitings, *row.NumRequestsWaiting)
		}
		if row.NumRequestsRunning != nil {
			runnings = append(runnings, *row.NumRequestsRunning)
		}
		if row.GPUCacheUsagePerc != nil {
			caches = append(caches, *row.GPUCacheUsagePerc)
		}
		if row.PromptTokensTotal != nil {
			if firstPrompt == nil {
				firstPrompt = row.PromptTokensTotal
			}
			lastPrompt = row.PromptTokensTotal
		}
		if row.GenerationTokensTot != nil {
			if firstGen == nil {
				firstGen = row.GenerationTokensTot
			}
			lastGen = row.GenerationTokensTot
		}
	}
	mergeMeanPeak(&s.gpuUtilMean, &s.gpuUtilPeak, utils)
	mergeMeanPeak(&s.gpuMemMeanMB, &s.gpuMemPeakMB, mems)
	mergeMeanPeak(&s.gpuPowerMeanW, &s.gpuPowerPeakW, powers)
	if len(waitings) > 0 {
		peak := maxOf(waitings)
		if s.waitingEverPeak == nil || peak > *s.waitingEverPeak {
			s.waitingEverPeak = &peak
		}
	}
	if len(runnings) > 0 {
		peak := maxOf(runnings)
		if s.runningPeak == nil || peak > *s.runningPeak {
			s.runningPeak = &peak
		}
	}
	if len(caches) > 0 {
		peak := maxOf(caches)
		if s.cacheUsagePeak == nil || peak > *s.cacheUsagePeak {
			s.cacheUsagePeak = &peak
		}
	}
	if firstPrompt != nil && lastPrompt != nil {
		d := *lastPrompt - *firstPrompt
		s.promptTokDelta = addOpt(s.promptTokDelta, d)
	}
	if firstGen != nil && lastGen != nil {
		d := *lastGen - *firstGen
		s.genTokDelta = addOpt(s.genTokDelta, d)
	}
}

func addOpt(existing *float64, v float64) *float64 {
	if existing == nil {
		return &v
	}
	sum := *existing + v
	return &sum
}

func mergeMeanPeak(meanOut, peakOut **float64, values []float64) {
	if len(values) == 0 {
		return
	}
	m := mean(values)
	p := maxOf(values)
	if *meanOut == nil {
		*meanOut = &m
	} else {
		avg := (**meanOut + m) / 2
		*meanOut = &avg
	}
	if *peakOut == nil || p > **peakOut {
		*peakOut = &p
	}
}

func percentile(values []float64, pct float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	idx := int(math.Ceil(pct/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func maxOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	m := values[0]
	for _, v := range values[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func stdev(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	m := mean(values)
	var sumSq float64
	for _, v := range values {
		sumSq += (v - m) * (v - m)
	}
	return math.Sqrt(sumSq / float64(len(values)-1))
}

// ---------------------------------------------------------------------
// Output: summary.csv, run.json, sample text, stdout report.
// ---------------------------------------------------------------------

func writeSummaryCSV(path string, order []groupKey, stats map[groupKey]groupStats) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create summary.csv: %w", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{
		"test", "node", "concurrency", "model", "repeats", "requests", "errors",
		"ttft_p50_ms", "ttft_p90_ms", "ttft_p99_ms", "ttft_max_ms",
		"decode_tok_s_p50", "decode_tok_s_p90", "itl_p50_ms", "itl_p90_ms",
		"node_aggregate_tok_s_mean", "node_aggregate_tok_s_stdev",
		"gpu_util_mean_pct", "gpu_util_peak_pct",
		"gpu_mem_mean_mb", "gpu_mem_peak_mb",
		"gpu_power_mean_w", "gpu_power_peak_w",
		"num_requests_waiting_peak", "num_requests_running_peak", "gpu_cache_usage_peak_pct",
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, k := range order {
		s := stats[k]
		row := []string{
			k.test, k.name, strconv.Itoa(k.concurrency), s.model,
			strconv.Itoa(s.repeats), strconv.Itoa(s.requestCount), strconv.Itoa(s.errorCount),
			f64(s.ttftP50), f64(s.ttftP90), f64(s.ttftP99), f64(s.ttftMax),
			f64(s.decodeTokSP50), f64(s.decodeTokSP90), f64(s.itlP50), f64(s.itlP90),
			f64(s.aggTokSMean), f64(s.aggTokSStdev),
			optF64(s.gpuUtilMean), optF64(s.gpuUtilPeak),
			optF64(s.gpuMemMeanMB), optF64(s.gpuMemPeakMB),
			optF64(s.gpuPowerMeanW), optF64(s.gpuPowerPeakW),
			optF64(s.waitingEverPeak), optF64(s.runningPeak), optF64(s.cacheUsagePeak),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

func f64(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
func optF64(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func writeRunJSON(path string, batches []batchResult) error {
	data, err := json.MarshalIndent(batches, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run.json: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write run.json: %w", err)
	}
	return nil
}

func writeSampleTexts(outDir string, order []groupKey, groups map[groupKey][]repeatRecord) {
	written := map[string]bool{}
	for _, k := range order {
		key := k.test + "_" + k.name
		if written[key] {
			continue
		}
		for _, rec := range groups[k] {
			for _, req := range rec.requests {
				if req.SampleText != "" {
					path := filepath.Join(outDir, fmt.Sprintf("sample_%s_%s.txt", k.test, k.name))
					_ = os.WriteFile(path, []byte(req.SampleText), 0o644)
					written[key] = true
					break
				}
			}
			if written[key] {
				break
			}
		}
	}
}

func printReport(order []groupKey, stats map[groupKey]groupStats, saturation string, expectedDailyRequests, typicalOutputTokens int) {
	byTest := map[string][]groupKey{}
	var tests []string
	for _, k := range order {
		if _, ok := byTest[k.test]; !ok {
			tests = append(tests, k.test)
		}
		byTest[k.test] = append(byTest[k.test], k)
	}
	sort.Strings(tests)

	const aggressiveTTFTMs = 2000
	const lenientTTFTMs = 5000

	for _, test := range tests {
		fmt.Printf("\n=== Test: %s ===\n", test)
		byNode := map[string][]groupKey{}
		var nodes []string
		for _, k := range byTest[test] {
			if _, ok := byNode[k.name]; !ok {
				nodes = append(nodes, k.name)
			}
			byNode[k.name] = append(byNode[k.name], k)
		}
		sort.Strings(nodes)

		for _, node := range nodes {
			keys := byNode[node]
			sort.Slice(keys, func(i, j int) bool { return keys[i].concurrency < keys[j].concurrency })
			fmt.Printf("\n-- %s (%s) --\n", node, stats[keys[0]].model)
			fmt.Printf("%-4s %8s %8s %8s %10s %10s %8s %10s %10s\n",
				"C", "ttftP50", "ttftP90", "ttftP99", "decodeTokS", "aggTokS", "errors", "gpuUtil%", "waitPeak")

			var aggressiveSaturatedAt, lenientSaturatedAt = -1, -1
			var c1AggTokS, lastAggTokS float64
			for _, k := range keys {
				s := stats[k]
				waitPeak := "-"
				if s.waitingEverPeak != nil {
					waitPeak = f64(*s.waitingEverPeak)
				}
				utilStr := "-"
				if s.gpuUtilMean != nil {
					utilStr = f64(*s.gpuUtilMean)
				}
				fmt.Printf("%-4d %8.1f %8.1f %8.1f %10.1f %10.1f %8d %10s %10s\n",
					k.concurrency, s.ttftP50, s.ttftP90, s.ttftP99, s.decodeTokSP50, s.aggTokSMean, s.errorCount, utilStr, waitPeak)

				breachedAggressive := s.ttftP90 > aggressiveTTFTMs || (s.waitingEverPeak != nil && *s.waitingEverPeak > 0)
				breachedLenient := s.ttftP90 > lenientTTFTMs || (s.waitingEverPeak != nil && *s.waitingEverPeak > 0)
				if breachedAggressive && aggressiveSaturatedAt == -1 {
					aggressiveSaturatedAt = k.concurrency
				}
				if breachedLenient && lenientSaturatedAt == -1 {
					lenientSaturatedAt = k.concurrency
				}
				if k.concurrency == 1 {
					c1AggTokS = s.aggTokSMean
				}
				lastAggTokS = s.aggTokSMean
			}

			aggLabel := describeSaturation(aggressiveSaturatedAt)
			lenLabel := describeSaturation(lenientSaturatedAt)
			fmt.Printf("saturation: aggressive(TTFTp90>%dms or queueing)=%s  lenient(TTFTp90>%dms or queueing)=%s\n",
				aggressiveTTFTMs, aggLabel, lenientTTFTMs, lenLabel)

			headlineSaturated := aggressiveSaturatedAt
			if saturation == "lenient" {
				headlineSaturated = lenientSaturatedAt
			}
			saturatedTokS := lastAggTokS
			if headlineSaturated == -1 {
				fmt.Printf("clustering-justification inputs: single-stream=%.1f tok/s, no saturation point reached within tested concurrency levels (headroom remains)\n", c1AggTokS)
			} else {
				fmt.Printf("clustering-justification inputs: single-stream=%.1f tok/s, saturated(%s) node throughput~=%.1f tok/s at C=%d\n",
					c1AggTokS, saturation, saturatedTokS, headlineSaturated)
			}
		}
	}

	fmt.Println("\n=== Interpretation ===")
	fmt.Println("Independent replicas (the same model loaded on each node, load-balanced)")
	fmt.Println("scale throughput ~linearly for parallel request load with none of clustering's")
	fmt.Println("complexity. Clustering (NCCL tensor/pipeline parallel over the fabric) is only")
	fmt.Println("justified when (a) one model must be larger than a single node's memory, (b)")
	fmt.Println("single-request latency must be lower than one node's compute delivers, or (c)")
	fmt.Println("you need one logical endpoint above one node's aggregate throughput and cannot")
	fmt.Println("load-balance replicas for your use case. Compare the per-node numbers above")
	fmt.Println("against your actual workload to see which, if any, apply.")

	if expectedDailyRequests > 0 && typicalOutputTokens > 0 {
		const activeHours = 8.0
		impliedRps := float64(expectedDailyRequests) / (activeHours * 3600)
		fmt.Printf("\n=== Headroom readout (assuming %g active hours/day) ===\n", activeHours)
		fmt.Printf("expected load ~= %.4f requests/sec, %d tokens/request typical output\n", impliedRps, typicalOutputTokens)
		fmt.Println("compare this against each node's saturated aggregate tok/s above to judge")
		fmt.Println("whether one node covers it, or how many replicas would be needed.")
	}
}

func describeSaturation(c int) string {
	if c == -1 {
		return "not reached"
	}
	return fmt.Sprintf("C=%d", c)
}
