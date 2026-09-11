# Plan: `scripts/dev-test.sh` - multi-node inference benchmark harness

Status: **all decisions settled (2026-09-11) - implementing**. This document is
the plan; the "Decisions for you" section below is now historical context for
each settled choice, not open questions.

### Settled so far (2026-09-10)

- **D3 Workload shape** = concurrency sweep.
- **D9 Implementation** = `scripts/dev-test.sh` (bash) + `scripts/fleetbench.go`
  (`//go:build ignore`, run via `go run`) for the concurrent SSE client / stats.
- **D8 GPU sampling** = SSH + `nvidia-smi` polling per node **and** scrape each
  vLLM `/metrics`.
- **D7 Generation knobs** = fixed length: `ignore_eos` + fixed `max_tokens`,
  `temperature: 0`, fixed `seed`; `stream: true` + `stream_options.include_usage`.

### Defaulted (from the doc's recommendations - veto any before implementation)

- **D1** targets = a config file (`--targets FILE`), `name url model ssh_host` per
  line; `--from-sparky` discovery is a possible later additive mode.
- **D2** endpoint = `/v1/chat/completions` (single user message); `--completions`
  flag to switch.
- **D10** output = stdout summary table + `dev-test-results/<UTC-ts>/` with
  `run.json`, `summary.csv`, truncated generated text; `dev-test-results/` added
  to `.gitignore`.
- **D11** model mismatch = report each target's model id, warn loudly if they
  differ, still run; `--require-same-model` to refuse.
- **D12** warmup = one discarded request per target; `--no-warmup` to skip.
- **D13** relationship to Sparky = fully standalone; does not start or need
  `sparky-server`.
- **D14** headroom readout = only when `--expected-daily-requests` /
  `--typical-output-tokens` are given; otherwise just print the curve.

### Settled (round 2, 2026-09-10)

- **D4** concurrency levels = `1 4 8 16 32` (auto-stop early on error or when
  TTFT p90 blows past the aggressive QoS threshold).
- **D5** = `R=3` repeats per level; latency reported as p50/p90/p99/max,
  headline throughput as mean +/- stdev.
- **QoS threshold** = the report **always shows both** an aggressive
  (TTFT p90 > 2s OR any `num_requests_waiting` > 0) and a lenient
  (TTFT p90 > 5s OR any queueing) saturation point, side by side; a
  `--saturation aggressive|lenient` flag only picks which one headlines the
  one-line bottom-of-report recommendation.

### Settled (round 3, 2026-09-11)

- **D6** - no hardcoded default scenario. `dev-test.sh` exposes named tests
  (`decode`, `prefill`; `all` runs both). Run with `--test name[,name...]|all`
  for non-interactive/scripted use; run with no `--test` flag on a TTY and it
  prompts an interactive picker instead. Running with no `--test` and no TTY
  (e.g. under cron/CI) is a hard usage error asking for `--test` explicitly,
  rather than silently guessing a default.

---

## 1. Goal

A dev/test tool, companion to `scripts/dev-server.sh` but otherwise independent
of it, that fires the **same workload at multiple inference endpoints
simultaneously** and reports, per endpoint, the numbers needed to decide whether
clustering some of these nodes for day-to-day workloads is worth the cost:

- Time to first token (TTFT)
- Tokens generated (prompt + completion)
- Decode throughput (tokens/sec) and end-to-end throughput
- Inter-token latency
- GPU utilization, GPU memory, (power draw if available) during the run
- How each of the above degrades as concurrent request load rises on a single node

Plus a synthesised "clustering justification" summary (section 4).

## 2. Non-goals

- Not a Sparky feature and not wired into `sparky-server`/`sparky-agent`. A
  standalone script under `scripts/`. It does not require the dev server to be
  running.
- Not a correctness/eval harness - it does not judge answer quality (beyond
  logging the text so you can eyeball it).
- Not a production load generator - modest scale (a handful of nodes, tens of
  concurrent requests), matching this project's stated scale.

## 3. Proposed design

### Flow

1. Read a **target list**: for each node, `{ name, base_url, model, ssh_host }`
   (see Decision D1 for how this is supplied).
2. Optional **warmup**: one discarded request per target (Decision D12).
3. For each **concurrency level** `C` in the configured list (Decision D3/D4),
   repeated `R` times (Decision D5):
   a. Start a per-node **GPU sampler**: a background `ssh <node> 'loop of
      nvidia-smi single-shot queries'` at a fixed interval (Decision D8),
      writing timestamped samples.
   b. Scrape each vLLM `/metrics` endpoint once (baseline) if enabled (D8).
   c. Fire `C` identical streaming requests at each node **concurrently**
      (all nodes and all C start together), each request timing TTFT and every
      token, and reading the final `usage` object.
   d. When every request for this (level, repeat) has finished: stop the GPU
      samplers, scrape `/metrics` again (delta), record the batch wall time.
4. Aggregate per (node, concurrency level): latency percentiles, per-request and
   **aggregate** throughput, GPU util/mem stats over the batch window, `/metrics`
   deltas.
5. Emit a stdout summary table + a machine-readable artifact (Decision D10).
6. Emit the clustering-justification summary (section 4).

### Metric definitions (precise, so the implementation is unambiguous)

Per request, client-measured from the SSE stream:

| Metric | Definition |
|---|---|
| `ttft_ms` | wall time from request send to first non-empty content delta |
| `total_ms` | wall time from request send to stream close |
| `prompt_tokens`, `completion_tokens` | from the final chunk's `usage` (needs `stream_options.include_usage=true`) |
| `decode_tok_s` | `completion_tokens / ((total_ms - ttft_ms) / 1000)` |
| `e2e_tok_s` | `completion_tokens / (total_ms / 1000)` |
| `itl_ms` | mean inter-token latency = `(total_ms - ttft_ms) / (completion_tokens - 1)` |

Per (node, concurrency level), over all `R x C` requests at that level:

- request count, error count (with HTTP status + body sample)
- `ttft_ms`: p50 / p90 / p99 / max
- `decode_tok_s`: per-request p50 / p90
- **`node_aggregate_tok_s`** = `sum(completion_tokens over the batch) / batch_wall_s`
  - the headline "how many tokens/sec can this one node serve at this load"
- `itl_ms`: p50 / p90
- GPU (from the SSH `nvidia-smi` samples inside the batch window): `util_pct`
  mean / peak / fraction-of-samples-at-100; `mem_used_mb` mean / peak (via
  `--query-compute-apps=used_memory` sum - `--query-gpu=memory.used` returns
  `[N/A]` on GB10, a known quirk); `power_w` mean / peak **if** `power.draw` is
  supported (verify on GB10 - may also be `[N/A]`)
- vLLM `/metrics` deltas/peaks if the endpoint exposes them: `num_requests_running`
  peak, `num_requests_waiting` peak (queue depth - the clearest saturation
  signal), `gpu_cache_usage_perc` peak, `prompt_tokens_total` /
  `generation_tokens_total` deltas (server-side cross-check of client counts),
  preemption count if present. Aphrodite exposes a subset; llama.cpp's server
  exposes different names or none - the script degrades gracefully and just omits
  what it can't get.

## 4. "Clustering justification" summary (synthesised from the above)

For each node and across nodes:

- **Single-stream decode tok/s** (`C=1`): what one interactive user gets.
- **Saturated node throughput**: `node_aggregate_tok_s` at the highest `C`
  before a chosen QoS threshold is breached (e.g. TTFT p90 > X ms, or
  `num_requests_waiting` > 0) - "what one node can actually serve day to day."
- **N independent replicas**: sum of each node's saturated throughput - the
  throughput you get from just running the same model on each node with a load
  balancer in front, **zero clustering**.
- **QoS degradation curve**: TTFT p90 and decode tok/s vs `C`, per node.
- **Interpretation the report will state plainly**: independent replicas scale
  throughput ~linearly for parallel request load with none of clustering's
  complexity. Clustering (NCCL tensor/pipeline parallel over the fabric) is only
  justified when (a) one model must be larger than a single node's memory, (b)
  single-request latency must be lower than one node's compute delivers, or
  (c) you need one logical endpoint above one node's aggregate throughput and
  cannot load-balance replicas for your use case. The script's numbers show
  which, if any, apply to your workload.
- **Optional headroom readout** (Decision D14): given an expected daily request
  volume + typical output length -> implied peak concurrency -> does one node's
  saturated throughput cover it (no clustering / no extra replicas needed), or
  how many replicas would.

---

## Decisions for you

Each is genuinely open. My recommendation is marked **[rec]** with reasoning; the
alternatives are real. Nothing here is so forced that I've picked it for you,
though D7-streaming and D7-usage are close (noted).

### D1 - How are targets specified?
- **[rec]** A small config file (e.g. `dev-test.targets` or a `--targets FILE`
  flag), one line per node: `name  base_url  model_id  ssh_host`. Explicit,
  reviewable, version-ignorable, works for endpoints Sparky doesn't manage (like
  the two running now).
- Alt A: CLI repeatable `--target name=...,url=...,model=...,ssh=...`. No file,
  more typing.
- Alt B: auto-discover from Sparky's API (`/dashboard/live-data` + nodes/instances)
  and only benchmark Sparky-managed instances. Convenient once instances are
  Sparky-loaded, useless for ad-hoc endpoints, couples the tool to Sparky.
- Alt C: file, with an optional `--from-sparky` mode layered on later.

### D2 - Which endpoint?
- **[rec]** `/v1/chat/completions` with a single user message - the realistic
  day-to-day shape; the chat template is applied server-side per model.
- Alt: `/v1/completions` with a raw prompt - identical bytes to every model, no
  template variance, but not how the models are actually used.
- (Could support both via a flag; default matters.)

### D3 - Workload shape
- **[rec]** A **concurrency sweep**: for `C` in a list, fire `C` concurrent
  identical requests per node; this is what actually informs a clustering
  decision (throughput ceiling + latency-under-load curve).
- Alt A: single stream per node (`C=1` only). Simple, measures raw decode speed,
  tells you nothing about how many users a node serves.
- Alt B: sustained fixed-duration load at one concurrency (e.g. 8 concurrent for
  5 min). Good for thermal/steady-state, worse for the scaling curve.
- Alt C: sweep + a final sustained run at the saturation point.

### D4 - Concurrency levels (only if D3 = sweep)
- **[rec]** `1 4 8 16 32` (stop early automatically if a level errors or TTFT p90
  blows past a ceiling).
- Alt: `1 2 4 8` (gentler), or a `--levels` flag with that as default.

### D5 - Repeats and reported statistics
- **[rec]** `R=3` repeats per level; report p50/p90/p99/max for latency,
  mean/peak for GPU, and mean +/- stdev for the headline throughput.
- Alt: `R=1` (fast, noisy), or `R=5` (slower, tighter). `--repeats` flag,
  default 3.

### D6 - Prompt
- **[rec]** Ship a default prompt that induces a few minutes of generation
  (e.g. "Write a detailed technical design document for X", targeting ~1500-2500
  output tokens), overridable with `--prompt-file FILE`. Same prompt to every
  node.
- Sub-decision: one prompt, or a small fixed set rotated across the `C` requests
  (more realistic mix, complicates comparison)? **[rec]** one prompt, identical
  everywhere, for clean comparison.
- Sub-decision: also run a **long-input** variant (e.g. 4-8k token prompt, short
  output) to measure prefill/TTFT separately from decode? **Settled round 3**:
  no hardcoded default - `decode` and `prefill` are both named, selectable
  tests (`all` runs both); `--test name[,name...]|all` for scripted use, an
  interactive picker when run on a TTY with no `--test` given, and a hard
  usage error (not a silent default) with neither a TTY nor `--test`.

### D7 - Generation knobs (fairness vs realism)
- `stream: true` - **required** for TTFT, not a choice.
- `stream_options.include_usage: true` - effectively required for exact token
  counts (the alternative, counting SSE chunks, is approximate). I'll use it.
- **[rec]** `temperature: 0`, fixed `seed`, fixed `max_tokens` (e.g. 2000), and
  `ignore_eos: true` (vLLM extension) so **every run generates exactly
  `max_tokens`** regardless of when the model would naturally stop - makes
  tokens-generated identical and the tok/s comparison apples-to-apples.
  - **Trade-off you should be aware of**: with `ignore_eos` you're measuring raw
    decode throughput for a fixed token budget, not "how it answers this
    prompt." If you want natural-length answers (variable token counts, real
    stop behaviour), drop `ignore_eos` and the comparison carries an
    output-length caveat.
  - `ignore_eos` is vLLM/Aphrodite-specific; llama.cpp's server uses `n_predict`
    and ignores unknown fields - the script would set it best-effort and note
    per-endpoint whether it took.

### D8 - GPU / power sampling
- **[rec]** SSH to each `ssh_host` and run a fixed-count loop of single-shot
  `nvidia-smi --query-gpu=utilization.gpu,power.draw --format=csv,noheader,nounits`
  (plus `--query-compute-apps=used_memory` for memory) at a fixed interval during
  each batch. This is the only way to get real SM utilization; vLLM's `/metrics`
  has cache usage but not GPU util.
  - Confirm: **SSH-based sampling is acceptable** (needs the same passwordless
    `developer@<node>` SSH we've been using). If not, GPU util is simply omitted
    and the report leans on `/metrics` + client timing.
  - Sample interval: **[rec]** 250 ms. `--gpu-sample-ms` flag.
  - Known GB10 quirks baked in: `nvidia-smi -lms`/`-l`/`-c` looping flags hang -
    use a counted loop of single-shot calls; `--query-gpu=memory.used` is
    `[N/A]` - use compute-apps sum; `power.draw` support is unverified - the
    script probes once and disables the column if it's `[N/A]`.
- **[rec]** Also scrape vLLM `/metrics` before/after each batch and once mid-batch
  (queue depth peak). Adds the server's own view (queueing, cache pressure,
  token counters) for near-zero cost. Skips cleanly for non-vLLM endpoints.
- Alt: client-only (no SSH, no `/metrics`) - simplest, but then "GPU utilization"
  (explicitly asked for) isn't in the report.

### D9 - Implementation language / structure
- **[rec]** `scripts/dev-test.sh` (bash entry point: arg parsing, target-file
  read, GPU-sampler SSH loops, orchestration) that runs a **single
  self-contained Go file** for the actual concurrent HTTP/SSE work and stats
  (`go run scripts/fleetbench.go`, the file carrying `//go:build ignore` so it
  stays out of `go build ./...`, `go vet ./...`, and the module's test surface).
  Rationale: matches the project's Go-first, dependency-light stance (CLAUDE.md);
  robust concurrent streaming + JSON + percentile math is painful in bash/curl
  and the project deliberately avoids Python; `dev-server.sh` already shells out
  to the Go toolchain.
- Alt A: pure `bash` + `curl --no-buffer` + `jq` + `awk`. No Go dependency at
  run time, but SSE parsing, per-token timing, concurrency, and percentiles in
  bash are fragile and hard to review.
- Alt B: `bash` + an embedded/sibling **Python 3** script (`asyncio` + stdlib
  HTTP). Cleanest streaming code, but adds a Python runtime expectation this repo
  otherwise doesn't have.
- Alt C: a real Go program at `cmd/fleetbench/` - part of the module (built by
  `go build ./...`, needs a license header + tests). Heavier footprint for a
  dev-only tool.

### D10 - Output
- **[rec]** Both: a human-readable summary table to stdout, and a timestamped
  artifact written to `dev-test-results/<UTC-timestamp>/` containing `run.json`
  (full per-request + aggregate data), `summary.csv` (one row per node x
  concurrency level), and the raw generated text per node (truncated). Add
  `dev-test-results/` to `.gitignore`.
- Sub-decisions: JSON vs CSV vs both (rec both); results dir location
  (rec repo-root `dev-test-results/`, gitignored) vs `/tmp` vs `--out DIR`.

### D11 - Model-mismatch handling
- **[rec]** The script reports each target's model id (from the `/v1/models`
  response) and **warns loudly** if they differ across targets, but still runs -
  comparing different models is sometimes what you want (e.g. "is the 27B on
  node B worth it vs the 8B on node A"). A `--require-same-model` flag makes it
  refuse instead.
- Alt: refuse by default; require `--allow-model-mismatch`.

### D12 - Warmup
- **[rec]** One warmup request per target, discarded, before the measured sweep
  (removes cold-cache / first-request-after-idle effects). `--no-warmup` to skip.

### D13 - Relationship to `dev-server.sh` / Sparky
- **[rec]** Fully standalone. `dev-test.sh` needs only: a target file and
  (for GPU sampling) SSH access. It does **not** start, need, or talk to
  `sparky-server`. This keeps it usable against any OpenAI-compatible endpoint,
  Sparky-managed or not.
- (If you later want `--from-sparky` discovery, that's an additive mode - D1
  Alt C.)

### D14 - Headroom readout inputs
- **[rec]** Optional flags `--expected-daily-requests N --typical-output-tokens M`
  (and an assumed active-hours span); when given, the report adds "implied peak
  concurrency ~= K, one node's saturated throughput covers/does not cover it,
  you would need ~P replicas." When omitted, the report just prints the curve
  and leaves the judgement to you.
- Alt: skip this entirely; the raw curve is enough.

---

## Open questions I can't answer myself

1. ~~Are the endpoints reachable from this laptop directly?~~ **Resolved
   2026-09-11**: yes - both `http://192.168.69.101:8000` and
   `http://192.168.69.102:8000` answer directly, no auth in front of vLLM.
2. ~~What ports / model ids are live right now?~~ **Resolved 2026-09-11**:
   both nodes serve on `:8000` via Sparky-managed profiles. Model ids (as
   `/v1/models` reports them - the raw filesystem path, per Sparky's own
   readiness-probe convention) are
   `/opt/sparky/serviceloop/models/gdubicki/Qwen3-Coder-Next-NVFP4-GB10`
   (Spark-1) and `/opt/sparky/serviceloop/models/Qwen/Qwen3-Coder-30B-A3B-Instruct-FP8`
   (Spark-2). These are two different model architectures/quant types, not
   the same model on two nodes - D11's warn-but-run handling applies.
3. **QoS threshold for "saturated"** - already resolved by design (see the
   QoS threshold entry under "Settled (round 2)" above): the report always
   shows both an aggressive and lenient threshold side by side rather than
   requiring the user to pick one in advance.

---

## Rough file layout (once decided)

```
scripts/
  dev-test.sh          # bash: args, target file, GPU-sampler SSH loops, orchestration, report
  fleetbench.go        # //go:build ignore - concurrent SSE client, timing, percentiles, JSON out
  dev-test.targets     # example/committed template (real one is gitignored or --targets)
dev-test-results/       # gitignored; per-run artifacts
.gitignore              # + dev-test-results/
```

No changes to `internal/`, `agent/`, `cmd/`, or the module's build/test surface.

## Verification plan (once built)

- `bash -n scripts/dev-test.sh` (syntax), `gofmt -l scripts/fleetbench.go`,
  `go vet` on the file via a throwaway build tag if practical.
- Dry-run mode (`--dry-run`) that prints the request plan and target list without
  firing anything.
- Real run against the two live endpoints (once ports confirmed): a single
  `C=1` scenario first to sanity-check TTFT/token/tps numbers against a manual
  `curl`, then the full sweep. Confirm the GPU samples line up with a manual
  `nvidia-smi` watch during the run.
- Confirm graceful handling: a target that's down (connection refused), a
  non-vLLM endpoint (no `/metrics`), an endpoint that ignores `ignore_eos`.
