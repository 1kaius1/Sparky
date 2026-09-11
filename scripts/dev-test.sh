#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Dev-only fleet benchmark harness - see PLAN-FLEET_PROMPT_TEST.md. Fires
# the same workload at multiple OpenAI-compatible inference endpoints
# simultaneously and reports TTFT, throughput, and GPU load per node, at a
# sweep of concurrency levels, to help decide whether clustering nodes for
# day-to-day workloads is worth the cost.
#
# Fully standalone (PLAN-FLEET_PROMPT_TEST.md D13): does not start, need, or
# talk to sparky-server. Orchestration, target-file handling, the menu, and
# SSH-based GPU/vLLM-metrics sampling live here in bash; the concurrent
# HTTP/SSE client and all percentile math live in scripts/fleetbench.go
# (invoked via `go run`, D9).
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FLEETBENCH="$SCRIPT_DIR/fleetbench.go"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

TARGETS_FILE="$SCRIPT_DIR/dev-test.targets"
TEST_SPEC=""
LEVELS="1 4 8 16 32"
REPEATS=3
MAX_TOKENS=2000
PREFILL_MAX_TOKENS=64
TEMPERATURE=0
SEED=0
GPU_SAMPLE_MS=250
TIMEOUT="5m"
NO_WARMUP=0
REQUIRE_SAME_MODEL=0
SATURATION="aggressive"
OUT_DIR=""
DRY_RUN=0
EXPECTED_DAILY_REQUESTS=0
TYPICAL_OUTPUT_TOKENS=0

usage() {
	cat <<EOF
Usage: $(basename "$0") [options]

  --targets FILE            target list (default: $TARGETS_FILE)
  --test NAME[,NAME...]|all which test(s) to run: decode, prefill, or all.
                             Omit on a TTY for an interactive picker; omitting
                             it with no TTY (e.g. cron) is a usage error.
  --levels "1 4 8 ..."       concurrency levels to sweep (default: $LEVELS)
  --repeats N                repeats per level (default: $REPEATS)
  --max-tokens N             decode-scenario max_tokens (default: $MAX_TOKENS)
  --gpu-sample-ms N          GPU/metrics sample interval (default: $GPU_SAMPLE_MS)
  --timeout DUR              per-request timeout, Go duration syntax (default: $TIMEOUT)
  --saturation aggressive|lenient   which QoS threshold headlines the recommendation (default: $SATURATION)
  --require-same-model       refuse to run if targets report different model ids
  --no-warmup                skip the one discarded warmup request per target
  --out DIR                  results directory (default: dev-test-results/<UTC-ts>/)
  --expected-daily-requests N   optional headroom readout input
  --typical-output-tokens N     optional headroom readout input
  --dry-run                  print the request plan and target list, do nothing
  -h, --help                 this help
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--targets) TARGETS_FILE="$2"; shift 2 ;;
	--test) TEST_SPEC="$2"; shift 2 ;;
	--levels) LEVELS="$2"; shift 2 ;;
	--repeats) REPEATS="$2"; shift 2 ;;
	--max-tokens) MAX_TOKENS="$2"; shift 2 ;;
	--gpu-sample-ms) GPU_SAMPLE_MS="$2"; shift 2 ;;
	--timeout) TIMEOUT="$2"; shift 2 ;;
	--saturation) SATURATION="$2"; shift 2 ;;
	--require-same-model) REQUIRE_SAME_MODEL=1; shift ;;
	--no-warmup) NO_WARMUP=1; shift ;;
	--out) OUT_DIR="$2"; shift 2 ;;
	--expected-daily-requests) EXPECTED_DAILY_REQUESTS="$2"; shift 2 ;;
	--typical-output-tokens) TYPICAL_OUTPUT_TOKENS="$2"; shift 2 ;;
	--dry-run) DRY_RUN=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown argument: $1" >&2; usage >&2; exit 1 ;;
	esac
done

if [ "$SATURATION" != "aggressive" ] && [ "$SATURATION" != "lenient" ]; then
	echo "error: --saturation must be aggressive or lenient" >&2
	exit 1
fi

if [ ! -f "$TARGETS_FILE" ]; then
	echo "error: target file not found: $TARGETS_FILE" >&2
	echo "copy scripts/dev-test.targets.example to $TARGETS_FILE and fill in real values, or pass --targets FILE" >&2
	exit 1
fi

# --- D6: resolve which test(s) to run - flag, interactive picker, or a hard
# usage error when neither a flag nor a TTY is available. ---
AVAILABLE_TESTS="decode prefill"
declare -a TESTS_TO_RUN=()

resolve_tests_from_spec() {
	local spec="$1"
	if [ "$spec" = "all" ]; then
		TESTS_TO_RUN=($AVAILABLE_TESTS)
		return 0
	fi
	local IFS=','
	local name
	for name in $spec; do
		case "$name" in
		decode | prefill) TESTS_TO_RUN+=("$name") ;;
		*) echo "error: unknown test '$name' (available: $AVAILABLE_TESTS, or 'all')" >&2; exit 1 ;;
		esac
	done
}

if [ -n "$TEST_SPEC" ]; then
	resolve_tests_from_spec "$TEST_SPEC"
elif [ -t 0 ]; then
	echo "Which test(s) to run?"
	select choice in decode prefill all; do
		case "$choice" in
		decode | prefill) TESTS_TO_RUN=("$choice"); break ;;
		all) TESTS_TO_RUN=($AVAILABLE_TESTS); break ;;
		*) echo "invalid choice" ;;
		esac
	done
else
	echo "error: --test is required when not running on a TTY (available: $AVAILABLE_TESTS, or 'all')" >&2
	exit 1
fi

# --- Load targets (name base_url ssh_host). ---
declare -a T_NAME=() T_URL=() T_SSH=()
while IFS= read -r line; do
	line="${line%%#*}"
	line="$(echo "$line" | xargs)"
	[ -z "$line" ] && continue
	read -r name url ssh_host <<<"$line"
	T_NAME+=("$name")
	T_URL+=("$url")
	T_SSH+=("$ssh_host")
done <"$TARGETS_FILE"

if [ "${#T_NAME[@]}" -eq 0 ]; then
	echo "error: no targets parsed from $TARGETS_FILE" >&2
	exit 1
fi

echo "Targets:"
for i in "${!T_NAME[@]}"; do
	echo "  ${T_NAME[$i]}  ${T_URL[$i]}  (gpu sampling via ${T_SSH[$i]:-none})"
done
echo "Tests: ${TESTS_TO_RUN[*]}"
echo "Concurrency levels: $LEVELS"
echo "Repeats per level: $REPEATS"

if [ "$DRY_RUN" -eq 1 ]; then
	echo
	echo "(dry run - nothing was sent)"
	exit 0
fi

# --- Pre-flight: resolve each target's live model id (D11). ---
echo
echo "Resolving live model ids..."
declare -a T_MODEL=()
FIRST_MODEL=""
MODEL_MISMATCH=0
for i in "${!T_NAME[@]}"; do
	model=$(curl -s -m 5 "${T_URL[$i]}/v1/models" | grep -o '"id":"[^"]*"' | head -1 | sed -E 's/"id":"([^"]*)"/\1/')
	if [ -z "$model" ]; then
		echo "  ${T_NAME[$i]}: could not resolve model id from ${T_URL[$i]}/v1/models" >&2
		exit 1
	fi
	T_MODEL+=("$model")
	echo "  ${T_NAME[$i]}: $model"
	if [ -z "$FIRST_MODEL" ]; then
		FIRST_MODEL="$model"
	elif [ "$model" != "$FIRST_MODEL" ]; then
		MODEL_MISMATCH=1
	fi
done
if [ "$MODEL_MISMATCH" -eq 1 ]; then
	if [ "$REQUIRE_SAME_MODEL" -eq 1 ]; then
		echo "error: targets report different model ids and --require-same-model was set" >&2
		exit 1
	fi
	echo "WARNING: targets report different model ids - this compares different models, not the same model on different hardware." >&2
fi

# --- Warmup (D12): one discarded request per target. ---
if [ "$NO_WARMUP" -eq 0 ]; then
	echo
	echo "Warming up..."
	for i in "${!T_NAME[@]}"; do
		curl -s -m 60 -X POST "${T_URL[$i]}/v1/chat/completions" \
			-H "Content-Type: application/json" \
			-d "{\"model\":\"${T_MODEL[$i]}\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\"max_tokens\":8,\"temperature\":0}" \
			>/dev/null 2>&1
		echo "  ${T_NAME[$i]}: done"
	done
fi

# --- Results directory. ---
TS="$(date -u +%Y%m%dT%H%M%SZ)"
if [ -z "$OUT_DIR" ]; then
	OUT_DIR="$REPO_ROOT/dev-test-results/$TS"
fi
mkdir -p "$OUT_DIR"
echo
echo "Results directory: $OUT_DIR"

# --- SSH ControlMaster setup, one persistent multiplexed connection per
# target with an ssh_host, reused by every sampler tick for the whole run
# (GB10's nvidia-smi looping flags hang, per PLAN-FLEET_PROMPT_TEST.md D8 -
# every sample is a fresh single-shot query, so a fresh SSH connection per
# tick would otherwise dominate the 250ms default interval). ---
declare -A CTRL_PATH=()
cleanup_ssh() {
	for host in "${!CTRL_PATH[@]}"; do
		ssh -o ControlPath="${CTRL_PATH[$host]}" -O exit "$host" >/dev/null 2>&1
	done
}
trap cleanup_ssh EXIT

for i in "${!T_NAME[@]}"; do
	host="${T_SSH[$i]}"
	[ -z "$host" ] && continue
	[ -n "${CTRL_PATH[$host]:-}" ] && continue
	ctrl="/tmp/dev-test-ssh-$$-$(echo "$host" | tr -c 'a-zA-Z0-9' '_')"
	if ssh -MNf -o ControlPath="$ctrl" -o ControlPersist=1h -o ConnectTimeout=5 "$host" 2>/dev/null; then
		CTRL_PATH[$host]="$ctrl"
	else
		echo "WARNING: could not establish SSH control connection to $host - GPU sampling for it will be skipped" >&2
	fi
done

# --- Per-tick sampler: one nvidia-smi round trip (via the shared multiplexed
# connection) plus one local curl to the target's /metrics, written as one
# CSV row. Runs as a background loop for the duration of one batch. ---
sample_target() {
	local ssh_host="$1" base_url="$2" out_csv="$3" interval_ms="$4"
	echo "epoch_ms,util_pct,mem_used_mb,power_w,num_requests_running,num_requests_waiting,gpu_cache_usage_perc,prompt_tokens_total,generation_tokens_total" >"$out_csv"
	local ctrl="${CTRL_PATH[$ssh_host]:-}"
	local sleep_s
	sleep_s=$(awk "BEGIN{printf \"%.3f\", $interval_ms/1000}")
	while true; do
		local ts util power mem gpu_line metrics_line nrr nrw gcu ptt gtt
		ts=$(date +%s%3N)
		util="" ; power="" ; mem=""
		if [ -n "$ctrl" ]; then
			gpu_line=$(ssh -o ControlPath="$ctrl" "$ssh_host" \
				"nvidia-smi --query-gpu=utilization.gpu,power.draw --format=csv,noheader,nounits 2>/dev/null; nvidia-smi --query-compute-apps=used_memory --format=csv,noheader,nounits 2>/dev/null | awk -F',' '{s+=\$1} END{print s+0}'" 2>/dev/null)
			util=$(echo "$gpu_line" | sed -n '1p' | cut -d',' -f1 | tr -d ' ')
			power=$(echo "$gpu_line" | sed -n '1p' | cut -d',' -f2 | tr -d ' ')
			mem=$(echo "$gpu_line" | sed -n '2p' | tr -d ' ')
			case "$util" in *N/A* | "") util="" ;; esac
			case "$power" in *N/A* | "") power="" ;; esac
		fi
		metrics_line=$(curl -s -m 1 "$base_url/metrics" 2>/dev/null)
		nrr=$(echo "$metrics_line" | grep -m1 '^vllm:num_requests_running' | awk '{print $NF}')
		nrw=$(echo "$metrics_line" | grep -m1 '^vllm:num_requests_waiting' | awk '{print $NF}')
		gcu=$(echo "$metrics_line" | grep -m1 -E '^vllm:(kv_cache_usage_perc|gpu_cache_usage_perc)' | awk '{print $NF}')
		ptt=$(echo "$metrics_line" | grep -m1 '^vllm:prompt_tokens_total' | awk '{print $NF}')
		gtt=$(echo "$metrics_line" | grep -m1 '^vllm:generation_tokens_total' | awk '{print $NF}')
		echo "${ts},${util},${mem},${power},${nrr},${nrw},${gcu},${ptt},${gtt}" >>"$out_csv"
		sleep "$sleep_s"
	done
}

run_batch() {
	local test="$1" concurrency="$2" repeat="$3" prompt_file="$4" max_tokens="$5"
	local batch_json="$OUT_DIR/batch_${test}_${concurrency}_${repeat}.json"

	declare -a sampler_pids=()
	for i in "${!T_NAME[@]}"; do
		local csv="$OUT_DIR/samples_${test}_${concurrency}_${repeat}_${T_NAME[$i]}.csv"
		sample_target "${T_SSH[$i]}" "${T_URL[$i]}" "$csv" "$GPU_SAMPLE_MS" &
		sampler_pids+=("$!")
	done

	go run "$FLEETBENCH" -mode run \
		-targets "$TARGETS_FILE" \
		-test "$test" \
		-concurrency "$concurrency" \
		-repeat "$repeat" \
		-prompt-file "$prompt_file" \
		-max-tokens "$max_tokens" \
		-temperature "$TEMPERATURE" \
		-seed "$SEED" \
		-ignore-eos \
		-timeout "$TIMEOUT" \
		>"$batch_json"
	local rc=$?

	for pid in "${sampler_pids[@]}"; do
		kill "$pid" >/dev/null 2>&1
	done
	wait "${sampler_pids[@]}" 2>/dev/null

	return $rc
}

echo
for test in "${TESTS_TO_RUN[@]}"; do
	case "$test" in
	decode)
		prompt_file="$SCRIPT_DIR/dev-test-prompts/decode.txt"
		tokens="$MAX_TOKENS"
		;;
	prefill)
		prompt_file="$SCRIPT_DIR/dev-test-prompts/prefill.txt"
		tokens="$PREFILL_MAX_TOKENS"
		;;
	esac

	for level in $LEVELS; do
		echo "== test=$test concurrency=$level =="
		aborted=0
		for repeat in $(seq 1 "$REPEATS"); do
			echo "  repeat $repeat/$REPEATS..."
			if ! run_batch "$test" "$level" "$repeat" "$prompt_file" "$tokens"; then
				echo "  WARNING: batch failed (test=$test concurrency=$level repeat=$repeat) - see $OUT_DIR" >&2
			fi
			# Auto-stop early on a clearly saturated/erroring level (D4):
			# if every request in this repeat errored, higher C is unlikely
			# to recover, so skip the remaining levels for this test.
			batch_json="$OUT_DIR/batch_${test}_${level}_${repeat}.json"
			if [ -f "$batch_json" ]; then
				total_reqs=$(grep -c '"index"' "$batch_json" 2>/dev/null)
				total_errs=$(grep -c '"error"' "$batch_json" 2>/dev/null)
				total_reqs="${total_reqs:-0}"
				total_errs="${total_errs:-0}"
				if [ "$total_reqs" -gt 0 ] && [ "$total_errs" -ge "$total_reqs" ]; then
					echo "  all requests errored at this level - stopping the sweep for test=$test early" >&2
					aborted=1
					break
				fi
			fi
		done
		[ "$aborted" -eq 1 ] && break
	done
done

echo
echo "Aggregating results..."
go run "$FLEETBENCH" -mode aggregate \
	-results-dir "$OUT_DIR" \
	-out-dir "$OUT_DIR" \
	-saturation "$SATURATION" \
	-expected-daily-requests "$EXPECTED_DAILY_REQUESTS" \
	-typical-output-tokens "$TYPICAL_OUTPUT_TOKENS"
