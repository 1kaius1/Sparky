#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Collects evidence for one real model launch through the packaged
# sparky-agent on a Docker-backend node (a DGX Spark). It does not drive the
# launch: the operator loads and unloads the profile from the Sparky UI when
# this script says so, and the script snapshots the node around each step.
#
# What it is meant to show, for the Known Issues rows a real launch closes:
#   - the per-launch GPU guard accepted a real agent-created container, and the
#     inspect shape that container reports (HostConfig.DeviceRequests);
#   - the engine came up, answers /v1/models, and GPU utilization rises while
#     it serves a request;
#   - serviceloop reaches the Docker socket (docker-group join);
#   - optionally: health reporting resumes after an agent restart with a model
#     loaded, and a container killed behind Sparky's back shows as Dead and is
#     cleared by Unload;
#   - after Unload, the container was archived and removed.
#
# Run it on ONE node that nobody else is using, from any directory:
#   ./collect_spark_launch_evidence.sh [output_dir]
# Output goes to ./tmp-spark_launch (or output_dir): spark_launch.log is the
# whole record. Copy that directory back and share it.
#
# It never prints or copies secrets.env (only the one backend line), and it
# reads the node's own state only: no step changes configuration. The two
# optional steps do disturb the loaded instance (an agent restart, a killed
# container) and each asks first.
#
# Every command that could hang is wrapped in timeout: nvidia-smi's -l/-lms
# flags hung on a Spark, so GPU samples are fixed-count single shots.

set -u -o pipefail

OUT_DIR="${1:-$PWD/tmp-spark_launch}"
LOG="$OUT_DIR/spark_launch.log"
# Tests point this at a file of canned answers; real use reads the terminal.
TTY_IN="${TTY_IN:-/dev/tty}"
AGENT_UNIT="${AGENT_UNIT:-sparky-agent}"
AGENT_BIN="${AGENT_BIN:-/opt/sparky/bin/sparky-agent}"
SERVICE_ACCOUNT="${SERVICE_ACCOUNT:-serviceloop}"
SECRETS_ENV="${SECRETS_ENV:-/etc/sparky-agent/secrets.env}"

# SUDO is empty when already root. An explicit SUDO="" in the environment
# also disables it (used by the tests with fake commands).
if [ -z "${SUDO+x}" ]; then
	if [ "$(id -u)" -eq 0 ]; then SUDO=""; else SUDO="sudo"; fi
fi

# How to run a command as the service account: runuser when already root
# (no password, no sudo needed), sudo otherwise. Overridable for tests.
if [ -z "${AS_SERVICE+x}" ]; then
	if [ "$(id -u)" -eq 0 ]; then AS_SERVICE="runuser -u $SERVICE_ACCOUNT --"; else AS_SERVICE="sudo -u $SERVICE_ACCOUNT"; fi
fi

# Opened once so a file of canned answers is consumed line by line.
exec 3< "$TTY_IN" || { echo "cannot read answers from $TTY_IN (run from a terminal)" >&2; exit 2; }

mkdir -p "$OUT_DIR" || { echo "cannot create $OUT_DIR" >&2; exit 2; }
: > "$LOG"

log() { printf '%s\n' "$*" | tee -a "$LOG"; }

section() { log ""; log "=================================================================="; log "== $*"; log "== $(date -u +%Y-%m-%dT%H:%M:%SZ)"; log "=================================================================="; }

# run SECONDS CMD...: echo the command, run it under a timeout, log its output
# and exit status. Never aborts the script: a failed command is evidence too.
run() {
	local t="$1"; shift
	log ""
	log "\$ $*"
	timeout "$t" "$@" 2>&1 | tee -a "$LOG"
	local rc=${PIPESTATUS[0]}
	[ "$rc" -ne 0 ] && log "[exit status $rc]"
	return 0
}

# runsh SECONDS 'shell string': same, for pipelines and redirections.
runsh() {
	local t="$1"; shift
	log ""
	log "\$ $*"
	timeout "$t" bash -c "$*" 2>&1 | tee -a "$LOG"
	local rc=${PIPESTATUS[0]}
	[ "$rc" -ne 0 ] && log "[exit status $rc]"
	return 0
}

# ask PROMPT: print a prompt, read one line, log both. Answer in $REPLY_TEXT.
ask() {
	{ printf '\n>>> %s ' "$1" > /dev/tty; } 2>/dev/null || printf '\n>>> %s ' "$1"
	REPLY_TEXT=""
	IFS= read -r -u 3 REPLY_TEXT || true
	printf '\n[prompt] %s\n[answer] %s\n' "$1" "$REPLY_TEXT" >> "$LOG"
}

yes_no() {
	ask "$1 [y/N]"
	case "$REPLY_TEXT" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

gpu_sample() {
	printf '%s  ' "$(date +%T)" | tee -a "$LOG"
	timeout 5 nvidia-smi --query-gpu=utilization.gpu,memory.used --format=csv,noheader 2>&1 | tee -a "$LOG"
}

gpu_idle_samples() {
	log ""
	log "GPU samples (utilization.gpu, memory.used), $1:"
	for _ in 1 2 3; do gpu_sample; sleep 1; done
}

# The newest running Sparky-managed container, unless CONTAINER is set.
find_container() {
	if [ -n "${CONTAINER:-}" ]; then echo "$CONTAINER"; return; fi
	timeout 20 $SUDO docker ps -q --filter label=sparky.managed=true --filter status=running 2>/dev/null | head -1
}

journal_since() {
	run 60 $SUDO journalctl -u "$AGENT_UNIT" --since "$1" --no-pager -o short-iso
	log ""
	log "Key lines from that journal slice (guard, health, failures):"
	local key
	key="$(timeout 60 $SUDO journalctl -u "$AGENT_UNIT" --since "$1" --no-pager -o short-iso 2>/dev/null \
		| grep -iE 'has no gpu|could not verify|fail|error|dead|unhealthy|check_instance|track|archiv|stopped|running')"
	log "${key:-(none)}"
}

container_facts() {
	local c="$1"
	section "Container facts: $c"
	run 20 $SUDO docker ps -a --filter "id=$c" --format 'table {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}'
	# The inspect shape the per-launch guard relies on, for a container the
	# agent created (the 2026-10-23 run only inspected a docker-run one).
	runsh 20 "$SUDO docker inspect '$c' --format 'DeviceRequests: {{json .HostConfig.DeviceRequests}}'"
	runsh 20 "$SUDO docker inspect '$c' --format 'Runtime: {{.HostConfig.Runtime}}  IpcMode: {{.HostConfig.IpcMode}}  ShmSize: {{.HostConfig.ShmSize}}  State: {{.State.Status}}  ExitCode: {{.State.ExitCode}}  OOMKilled: {{.State.OOMKilled}}'"
	runsh 20 "$SUDO docker inspect '$c' --format 'Labels: {{json .Config.Labels}}'"
	# Only the NVIDIA_* variables: the engine's full environment is not needed.
	runsh 20 "$SUDO docker inspect '$c' --format '{{range .Config.Env}}{{println .}}{{end}}' | grep '^NVIDIA'"
	run 30 $SUDO docker exec "$c" nvidia-smi -L
	log ""
	log "Last 40 lines of the container's own output:"
	runsh 30 "$SUDO docker logs --tail 40 '$c' 2>&1"
}

# Fire one real request at the engine and sample the GPU while it runs.
serve_and_sample() {
	local c="$1" port model
	port="$(timeout 20 $SUDO docker inspect "$c" --format '{{range $p,$b := .NetworkSettings.Ports}}{{range $b}}{{.HostPort}} {{end}}{{end}}' 2>/dev/null | awk '{print $1}')"
	section "Engine reachability and GPU use while serving (host port: ${port:-unknown})"
	if [ -z "$port" ]; then log "no published host port found; skipping"; return; fi

	runsh 20 "curl -sS --max-time 15 http://127.0.0.1:$port/v1/models"
	model="$(timeout 20 curl -sS --max-time 15 "http://127.0.0.1:$port/v1/models" 2>/dev/null | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)"
	log ""
	log "model id taken from /v1/models: ${model:-<none>}"
	if [ -z "$model" ]; then log "engine did not report a model id; skipping the completion"; return; fi

	gpu_idle_samples "idle, before the request"

	log ""
	log "Sending one chat completion (max_tokens 256, temperature 0) and sampling the GPU about once a second:"
	local body="{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"Write a long, detailed story about a robot learning to paint.\"}],\"max_tokens\":256,\"temperature\":0}"
	curl -sS --max-time 180 -o "$OUT_DIR/completion.json" -w 'completion: http %{http_code} in %{time_total}s\n' \
		-H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$port/v1/chat/completions" 2>&1 | tee -a "$LOG" &
	local cpid=$!
	local i
	for i in $(seq 1 60); do
		gpu_sample
		kill -0 "$cpid" 2>/dev/null || break
		sleep 1
	done
	wait "$cpid" 2>/dev/null
	log ""
	log "Start of the completion response:"
	head -c 500 "$OUT_DIR/completion.json" 2>/dev/null | tee -a "$LOG"
	log ""
}

# ------------------------------------------------------------------ start

section "Spark launch evidence - $(basename "$0")"
log "Output directory: $OUT_DIR"
log "Run by: $(id -un) on $(hostname)"
log ""
log "Use ONE node that nobody else is using. The loaded profile should be a small"
log "model that loads quickly. Nothing below changes configuration; the optional"
log "steps disturb the loaded instance and ask first."

if ! yes_no "Is this a node nobody else is using right now?"; then
	log "Not confirmed; stopping."
	exit 1
fi

ask "Which profile will you launch (name, engine, model)? Free text, Enter to skip:"
ask "Which node name does the Sparky UI call this machine? Free text, Enter to skip:"

# -------------------------------------------------------------- phase 0

section "Phase 0 - baseline of the node before the launch"
run 10 uname -srm
runsh 10 "head -2 /etc/os-release"
# The serial number is not needed and not shared.
runsh 10 "grep -v SERIAL /etc/dgx-release"
run 20 $SUDO docker version --format 'docker server {{.Server.Version}}'
runsh 20 "$SUDO docker info --format '{{range \$k,\$v := .Runtimes}}{{\$k}} {{end}}'"
run 10 nvidia-smi -L

section "Installed agent"
runsh 20 "dpkg-query -W -f='\${Package} \${Version}\n' sparky-agent 2>/dev/null || rpm -q sparky-agent 2>&1"
runsh 10 "ls -l '$AGENT_BIN'; sha256sum '$AGENT_BIN'"
run 10 $SUDO systemctl is-active "$AGENT_UNIT"
run 10 $SUDO systemctl show "$AGENT_UNIT" -p ActiveEnterTimestamp -p NRestarts
# Only the backend line: secrets.env also holds the bearer token.
runsh 10 "$SUDO grep -E '^SPARKY_RUNTIME_BACKEND=' '$SECRETS_ENV'"

section "Service account and Docker socket access"
run 10 id "$SERVICE_ACCOUNT"
runsh 10 "getent group docker"

section "check-gpu as the service account"
PROBE_IMAGE="${PROBE_IMAGE:-}"
if [ -z "$PROBE_IMAGE" ]; then
	# The Sparks are airgapped: use an image already on the node.
	PROBE_IMAGE="$(timeout 20 $SUDO docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -i vllm | head -1)"
fi
if [ -n "$PROBE_IMAGE" ]; then
	log "probe image: $PROBE_IMAGE (set PROBE_IMAGE to override)"
	runsh 180 "cd / && $SUDO $AS_SERVICE '$AGENT_BIN' check-gpu --image '$PROBE_IMAGE'; echo \"check-gpu exit status: \$?\""
else
	log "no local vllm image found and PROBE_IMAGE not set; skipping check-gpu"
fi

section "Sparky-managed containers already on this node"
run 20 $SUDO docker ps -a --filter label=sparky.managed=true --format 'table {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}'
gpu_idle_samples "idle baseline"

# -------------------------------------------------------------- phase A

T0="$(date '+%Y-%m-%d %H:%M:%S')"
log ""
log "Journal marker for this run: $T0"

section "Phase A - launch"
log "Now, in the Sparky UI, Load the profile on THIS node."
ask "Press Enter when the instance shows Running (or Failed), then describe what the UI shows:"

section "After the launch: agent journal"
journal_since "$T0"

C="$(find_container)"
if [ -z "$C" ]; then
	section "No running Sparky container found"
	run 20 $SUDO docker ps -a --filter label=sparky.managed=true --format 'table {{.ID}}\t{{.Names}}\t{{.Status}}'
	log "No running managed container: the launch probably failed (the journal above"
	log "should say why). Skipping the container steps."
else
	container_facts "$C"
	serve_and_sample "$C"
	gpu_idle_samples "after the request"
fi

# -------------------------------------------------------------- phase B

if [ -n "$C" ]; then
	section "Phase B (optional) - agent restart with the model loaded"
	log "Checks that the container survives an agent restart and that the Dashboard"
	log "health keeps updating afterward (health-resume)."
	if yes_no "Restart the agent now?"; then
		TB="$(date '+%Y-%m-%d %H:%M:%S')"
		run 60 $SUDO systemctl restart "$AGENT_UNIT"
		log "waiting 20 seconds for the agent to reconnect..."
		sleep 20
		run 10 $SUDO systemctl is-active "$AGENT_UNIT"
		run 20 $SUDO docker ps --filter "id=$C" --format 'table {{.ID}}\t{{.Status}}'
		journal_since "$TB"
		log ""
		log "In the UI, watch the instance's health for about 2 minutes."
		ask "Does the Dashboard health keep updating (last check time moving)? Describe, then Enter:"
	else
		log "Skipped by the operator."
	fi
fi

# -------------------------------------------------------------- phase C

if [ -n "$C" ]; then
	section "Phase C (optional) - container killed behind Sparky's back (Dead state)"
	log "Kills the engine container with docker kill. Expected: within the health"
	log "interval (60 seconds by default) the instance shows Dead in red, and"
	log "Unload in the next phase clears it."
	if yes_no "Kill the container now?"; then
		TC="$(date '+%Y-%m-%d %H:%M:%S')"
		run 30 $SUDO docker kill "$C"
		run 20 $SUDO docker ps -a --filter "id=$C" --format 'table {{.ID}}\t{{.Status}}'
		log "waiting 90 seconds for the next health check..."
		sleep 90
		journal_since "$TC"
		ask "Does the UI show the instance as Dead (red)? Describe, then Enter:"
	else
		log "Skipped by the operator."
	fi
fi

# -------------------------------------------------------------- phase D

section "Phase D - unload"
TD="$(date '+%Y-%m-%d %H:%M:%S')"
log "Now, in the Sparky UI, Unload the instance."
ask "Press Enter when the UI shows it stopped (or cleared), then describe what it shows:"

section "After the unload"
journal_since "$TD"
run 20 $SUDO docker ps -a --filter label=sparky.managed=true --format 'table {{.ID}}\t{{.Names}}\t{{.Status}}'
if [ -n "$C" ]; then
	# The agent reports stopped first and removes the container only after the
	# central app confirms the archived log, so allow it up to 40 seconds.
	log ""
	log "Waiting for the container to be archived and removed (up to 40 seconds)..."
	gone=""
	for _ in $(seq 1 20); do
		if [ -z "$(timeout 20 $SUDO docker ps -a --filter "id=$C" -q 2>/dev/null)" ]; then gone=1; break; fi
		sleep 2
	done
	if [ -n "$gone" ]; then
		log "container $C is gone (archived and removed, as designed)"
	else
		log "container $C STILL EXISTS after 40 seconds (the archive upload may not have been confirmed)"
		run 20 $SUDO docker ps -a --filter "id=$C" --format 'table {{.ID}}\t{{.Status}}'
	fi
fi
gpu_idle_samples "after the unload (memory should be back near the idle baseline)"
ask "Does the Container logs page show an archive for this run? Describe, then Enter:"

section "Done"
log "Everything is in: $LOG"
log "Copy the directory back, for example from your laptop:"
log "  scp -r $(id -un)@$(hostname):$OUT_DIR ."
log "Skim it before sharing: it holds hostnames, image names and the model's"
log "generated text, but no secrets.env content."
