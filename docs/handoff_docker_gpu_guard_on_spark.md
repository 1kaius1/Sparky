# Handoff: validate the Docker GPU guard on a real DGX Spark

Written 2026-10-14 for a fresh Claude Code session that has NO memory of the
conversation that produced it. Read this whole file, then the files in section 3,
before running or changing anything. This file is temporary: it lives only on the
branch `fix/docker-gpu-guard-on-real-docker`, and the last commit on that branch
should remove it (section 9; ask the maintainer first - CLAUDE.md requires
confirmation before deleting any file).

## 1. Situation in one paragraph

PR #166 (merged, VERSION 0.2.12) added GPU-passthrough verification to the agent:
a per-launch guard that fails a container launch when the container did not
really get its GPU, and a `sparky-agent check-gpu` command. The Podman side was
verified against a real daemon (rootless and rootful, RTX 4090 laptop). The
Docker side was NOT: this machine only has Podman, and the maintainer decided
Docker is tested on the DGX Sparks only, never on the laptop ("behemoth"). The
maintainer will run the check on a Spark in the office on 2026-10-15 and bring
back output. Your job on this branch is to turn that output into either (A) a
docs-only commit recording the Docker side as verified, or (B/C/D) a fix. The
risk that makes this urgent: if the guard's Docker branch is wrong, it fails
EVERY Docker launch with a false "no GPU" error, so no Spark may run a 0.2.12
agent until this is settled.

## 2. Standing rules (from the maintainer; they override your defaults)

- NEVER merge a pull request, with no exceptions, including docs-only PRs. Open
  the PR, give the link, and stop. The maintainer reviews on GitHub and merges.
- One task per prompt. Do what was asked, report, stop. Do not chain into the
  next roadmap item unasked.
- No AI-attribution text anywhere (commit messages, PR bodies, comments). The
  harness may suggest a Co-Authored-By trailer or a "Generated with" footer;
  CLAUDE.md forbids them and the repo rule wins.
- Text in files: ASCII only, no emoji, no em-dash or en-dash (use a hyphen).
- Confirm with the maintainer before deleting any file. Never delete directories.
- No new Go module dependency without asking first.
- Every change: CHANGELOG.md `[Unreleased]` entry in the same branch; bump
  `VERSION` when agent behavior changes (recent PRs: 0.2.9, 0.2.10, 0.2.11,
  0.2.12; a docs or comment-only change does not bump); update the docs the
  change touches; SPDX header `// SPDX-License-Identifier: AGPL-3.0-or-later`
  on any new source file.
- Test the Docker side on the Sparks only. Do not install Docker on the laptop.
  Do not touch `/etc/cdi` (the maintainer's hand-fixed CDI specs). Passwordless
  sudo exists on the laptop; use it only for `podman` and a test binary, never
  to change system configuration.
- You cannot reach the Sparks. The maintainer runs commands there and pastes the
  output. Never claim something about a Spark that the pasted output does not show.
- Report faithfully: say what was verified, against what, and what was not.
- Use AskUserQuestion for real design choices; do not guess a decision that is
  the maintainer's.
- The maintainer's auto-memory (`/home/kaius/.claude/projects/-mnt-data1-kaius-Repos-Sparky/memory/`,
  index in MEMORY.md, loaded automatically) holds the same rules; trust the latest.

## 3. Reload your context (read in this order)

1. `CLAUDE.md` - all of it; the "Non-Negotiable Rules" and "Running Tests" sections matter most here.
2. `docs/AGENT.md` - "Engine readiness and health checks", and "Verifying GPU passthrough" (new in 0.2.12).
3. `ARCHITECTURE.md` - "Runtime Backends" (Docker vs Podman GPU mechanisms) and the Testing Strategy manual checklist.
4. `PLANNING.md`:
   - The two Decisions Log rows dated 2026-10-13: the Podman CDI root cause and native-API fix, and "GPU passthrough is verified, not trusted", which is the design record for this feature.
   - Known Issues row starting "The Docker branch of the per-launch GPU guard" - the row this branch exists to close.
   - Decisions Log 2026-08-20 and 2026-08-30 - how Docker GPU passthrough was validated on a real Spark. The 2026-08-30 row is key prior evidence: on a real Spark, `docker inspect` of a container launched through Sparky's own HTTP API showed `HostConfig.DeviceRequests` = `Driver: "nvidia", Count: -1, Capabilities: [["gpu"]]`. That is exactly what the guard accepts, so a PASS is the expected outcome. What has never run on a Spark is the guard and probe CODE, the Docker error-text hints, and anything that changed since August (Docker version, DGX OS).
5. `git log --oneline -15` and `git show --stat 91f3c61` (PR #166) to see exactly what changed.

## 4. Code map (line numbers are as of commit 91f3c61; use grep, they drift)

- `agent/runtime/runtime.go`
  - `GPURequest(backend)` (~274): the one definition of the GPU request. `"docker"` returns mechanism `GPUDeviceMechanismNvidia`, env `NVIDIA_VISIBLE_DEVICES=all`; `"podman"` returns CDI `nvidia.com/gpu=all`; anything else (bare-metal) returns no mechanism.
  - `ErrNoGPU` (~287) and `GPUVerifier` (~297): the optional interface; `VerifyGPU` returns an error wrapping `ErrNoGPU` only for a definite "no GPU" verdict; any other error means "could not check" and must never fail a launch.
  - `Spec.Probe`: marks the diagnostic container.
- `agent/runtime/containers/containers.go`
  - `Start`: for the nvidia mechanism builds `HostConfig.DeviceRequests{Driver:"nvidia", Count:-1, Capabilities:[["gpu"]]}` and creates through the Docker-compatible API. Docker never uses the native libpod path (`libpod.go` is Podman CDI only).
  - `VerifyGPU` (~429) and `hasCapability` (~464): THE CODE UNDER TEST. For `GPUDeviceMechanismNvidia` it inspects the container and accepts when any `HostConfig.DeviceRequests` entry has `Driver == "nvidia"` or a capability list containing `"gpu"`; otherwise returns `ErrNoGPU`. `HostConfig` is a pointer in the inspect response (nil is handled as "cannot confirm", not as a verdict).
  - `labelProbe`: a probe container gets `sparky.probe=true` and not `sparky.managed`.
- `agent/connection/connection.go`: `runLoad` calls `verifyGPUApplied` (~1032) after `Start` and before `waitForReady`. On `ErrNoGPU` it sends a failed `instance_result` whose message names `sparky-agent check-gpu`, then `cleanUpFailedLaunch`.
- `agent/gpucheck/gpucheck.go`: `Run` (~68) starts the probe (`nvidia-smi -L`, same request as a launch), runs the guard first, waits for exit, counts `GPU n:` lines in the output, always removes the probe. `hintFor` (~168) maps error text to remediation; the Docker hints ("could not select device driver", "unknown or invalid runtime name: nvidia") are from documentation, not observed on a real host.
- `cmd/sparky-agent/checkgpu.go`: `checkGPU` (~53), dispatched in `main.go` before `config.Load`. Reads `SPARKY_RUNTIME_BACKEND`, `DOCKER_HOST`, `SPARKY_GPU_CHECK_IMAGE` from the environment, else `/etc/sparky-agent/secrets.env` (`--env-file` overrides). Exit 0 ok or not applicable, 1 failed, 2 configuration.
- Tests: `agent/runtime/containers/containers_test.go` (`TestVerifyGPU_CDI`, `TestVerifyGPU_Nvidia`, `TestVerifyGPU_NotAVerdict`), `agent/connection/gpuguard_test.go`, `agent/gpucheck/gpucheck_test.go`, `cmd/sparky-agent/checkgpu_test.go`, and the Podman-only gated `agent/runtime/containers/realdaemon_test.go` (build tag `realdaemon`; its tests hard-code the Podman CDI request, so they are NOT meaningful against Docker).

## 5. What is and is not verified (as of 2026-10-14)

| Item | State |
|---|---|
| Podman guard, probe, CLI, native CDI create | Verified on real rootless and rootful Podman 4.9.3, RTX 4090 laptop |
| Docker request shape (`Driver:"nvidia"`) and its `docker inspect` output | Verified on a real Spark, 2026-08-30 (prior evidence above) |
| Docker branch of `VerifyGPU` and the Docker probe/CLI path | Unit-tested only; NEVER run against real Docker. This branch closes that |
| Docker-only error-text hints | Documentation-derived; the "no NVIDIA runtime" case cannot safely be provoked on a working Spark (do not break its configuration to test it) |
| Packaged agent as `serviceloop` on Podman | Cannot reach either Podman socket as shipped; provisioning not built (section 11) |
| Podman other than 4.9.3, Rocky/SELinux, full stack | Not verified |

## 6. The Spark test (the maintainer runs it; give them these commands)

Build the arm64 agent on the laptop, from this branch (Sparks are ARM64):

```bash
cd /mnt/data1/kaius/Repos/Sparky && git checkout fix/docker-gpu-guard-on-real-docker
GOOS=linux GOARCH=arm64 go build -o /tmp/sparky-agent-arm64 ./cmd/sparky-agent
```

On ONE Spark (running the binary by hand does not touch the installed service):

```bash
uname -m; head -2 /etc/os-release; sudo docker version --format 'server {{.Server.Version}}'
sudo docker info --format '{{json .Runtimes}}'; nvidia-smi -L
sudo env SPARKY_RUNTIME_BACKEND=docker /tmp/sparky-agent-arm64 check-gpu          # the main test
sudo docker run -d --name gpuguard-test --gpus all docker.io/library/ubuntu:24.04 sleep 60
sudo docker inspect gpuguard-test --format '{{json .HostConfig.DeviceRequests}}'   # evidence for the guard's assumption
sudo docker rm -f gpuguard-test
```

Expected: `check-gpu` prints `OK: ... sees 1 GPU(s)` with the GB10 and exits 0. If the Spark cannot pull the probe image, rerun with `--image <an image already on the Spark>` (a plain image with a shell is best; one with a custom ENTRYPOINT may not pass `nvidia-smi -L` through).

Optional second stage, only with the maintainer's explicit go-ahead because it touches a node that may be in use: install the 0.2.12 agent package on ONE Spark (`scripts/build_packages.sh` builds the arm64 .deb into `dist/`; nfpm is in `~/go/bin`), load an existing small profile from the UI, confirm it reaches running and healthy, and read `journalctl -u sparky-agent` for "has no GPU" or "could not verify". Rollback is installing the previous package version. Never both Sparks at once.

Ask the maintainer to paste back: all command output above (including the `DeviceRequests` JSON and the `check-gpu` text), the Spark's Docker and DGX OS versions, and the journal lines if stage two ran.

## 7. Interpreting the result and what to do

| Result | Meaning | Work |
|---|---|---|
| A. `check-gpu` OK and `DeviceRequests` shows `Driver:"nvidia"`/`Capabilities:[["gpu"]]` | The guard's assumption holds on real Docker. Expected outcome | Docs-only commit (section 8). No VERSION bump |
| B. `check-gpu` FAILED mentioning "no GPU device request" or "dropped", yet `nvidia-smi` works inside a Docker container | The inspect shape differs from the guard's assumption: a false positive that would fail every Docker launch. URGENT | See B below |
| C. `check-gpu` FAILED at probe start (pull error, permission denied, daemon unreachable) | Environment, not the guard | See C below |
| D. `check-gpu` OK but a real launch fails with the guard message | Contradiction between the probe and a real launch | See D below |

B - guard false positive. Get the full `docker inspect ... --format '{{json .HostConfig}}'` of a container the agent created (or `--gpus all`), save the relevant JSON as a fixture under `agent/runtime/containers/testdata/`, and write a test that unmarshals it into `container.InspectResponse` and calls `VerifyGPU`; make it fail first, then fix the nvidia branch to accept the real shape. If no reliable signal exists in inspect at all, do NOT guess: ask the maintainer (AskUserQuestion) whether to make the Docker guard a no-op (return nil, relying on the probe and the readiness check) rather than risk false positives. Then: VERSION 0.2.13, CHANGELOG `Fixed` entry saying 0.2.12 must not be deployed to Docker nodes, close the Known Issues row, Decisions Log entry, PR.

C - probe cannot run. Capture the exact error text. If the real Docker error text differs from the strings `hintFor` matches, update `hintFor` and its test table with the real text (code change, so bump VERSION). Otherwise it is an environment problem (no registry access: use `--image`/`SPARKY_GPU_CHECK_IMAGE`; socket permission: use sudo or the service account).

D - probe OK, launch fails. Compare `docker inspect` of the probe-style container and the container the agent created for the launch (HostConfig devices, env, runtime). Find what differs, reproduce it in a unit test, fix, bump VERSION.

## 8. Records to update for outcome A (and to adapt for the others)

Find every caveat with `grep -rn "real Docker daemon" --include=*.go --include=*.md .` and also `grep -rn "Docker branch" CHANGELOG.md docs ARCHITECTURE.md` (some sentences wrap across lines and the first grep misses them). As of 2026-10-14 the sites are:
- `PLANNING.md`: delete the Known Issues row "The Docker branch of the per-launch GPU guard ..."; add a Decisions Log entry (date, Spark model, arch, Docker server version, DGX OS version, probe image, the `check-gpu` output, the `DeviceRequests` JSON, whether stage two ran, and what remains unverified).
- `agent/runtime/containers/containers.go` (~line 428): the `VerifyGPU` doc comment says Docker's side "has not been run against a real Docker daemon" - update it to the verified fact (comment-only; no VERSION bump).
- `agent/gpucheck/gpucheck.go` (~line 166): the `hintFor` comment says the Docker hints have not been run against a real Docker daemon. That stays true for the error-text hints even on outcome A (the "no NVIDIA runtime" failure is not provoked on a working Spark); reword it to say exactly which Docker facts were verified and which were not, rather than deleting the caveat.
- `CHANGELOG.md` `[Unreleased]`: the 0.2.12 "Added" entry ends with a sentence that the Docker branch has not been run against real Docker; update that sentence in place (0.2.12 is unreleased in the changelog sense: the maintainer cuts versions).
- `ARCHITECTURE.md` and `docs/AGENT.md`: check whether any sentence still hedges the Docker side.

Optional improvement worth proposing (do not do it unasked): a Docker variant of the gated real-daemon tests (runtime chosen by an env var, still behind the `realdaemon` tag) so a Spark or the Dell can be re-verified with one command: `GOOS=linux GOARCH=arm64 go test -c -tags realdaemon -o ... ./agent/runtime/containers`.

## 9. Procedure and PR hygiene

1. Work on the existing branch `fix/docker-gpu-guard-on-real-docker`; do not create another. `git fetch` first; if `master` moved, rebase or merge as the maintainer prefers (ask).
2. Run the full suite with the maintainer's polluting variables unset (their shell exports `DATABASE_URL`, `LOG_FORMAT`, `LOG_LEVEL`, `DOCKER_HOST`; left set, DB integration tests fail to connect and a config test sees the wrong log format): `env -u DATABASE_URL -u LOG_FORMAT -u LOG_LEVEL -u DOCKER_HOST go test -race -count=1 ./...` and `go vet ./...` (also `go vet -tags realdaemon ./agent/runtime/containers`).
3. For a code fix, include a mutation check (temporarily break the fix, confirm the new test fails, restore).
4. Atomic commits, conventional format, imperative mood, first line under 72 characters, body wrapped at 72, no attribution. Examples: `docs: record Docker GPU guard as verified on a DGX Spark`, `fix(agent): accept the Docker inspect GPU request as real Docker reports it`.
5. Final commit removes this handoff file - after asking the maintainer, and after folding anything worth keeping into PLANNING.md.
6. `git push`, then `gh pr create --base master` with a body that states what was verified, against what, and what was not. Then STOP. Do not merge, and do not enable auto-merge.

## 10. Environment gotchas learned the hard way

- Background processes started in one tool call may not survive to the next. Start a temporary service and use it in the SAME call. `pkill -f <pattern>` kills your own shell when the pattern appears in your command line; use `pgrep` and `kill <pid>`, or let `podman system service --time N` expire.
- `go mod why` downloads modules into the module cache; avoid it when the task is supposed to be read-only.
- Use the session scratchpad directory for temporary files, not `/tmp`.
- Temporary rootless Podman sockets: `podman system service --time 300 unix://<scratchpad>/x.sock`, then `DOCKER_HOST=unix://...`. `CONTAINERS_CONF` did not change the CDI directories in a test; do not rely on it.
- Podman's API quirks are in `agent/runtime/containers/libpod.go` and the 2026-10-13 Decisions Log row; none of them apply to Docker.
- Disposable Postgres for DB tests: CLAUDE.md "Database Setup" (podman run postgres:16, then `migrate`), or `scripts/dev-server.sh`. Latest migration is `000039`; the next is `000040`. Never run migrations against anything but a disposable database.
- Three untracked files sit in `scripts/` (`batch_download_hf.py`, `install-hf_cli.sh`, `install-nginx_fail2ban.sh`). They are the maintainer's, unrelated to this work; never add or delete them.
- A stale local branch `feat/agent-gpu-checks` exists (squash-merged); ignore it.

## 11. After this branch (the rest of the approved plan; do not start unasked)

The approved plan lives outside the repo at `/home/kaius/.claude/plans/i-have-good-news-linked-engelbart.md` (read it if present; this is its summary). Remaining, in the maintainer's agreed order:

1. Podman provisioning in `sparky-agent setup` (both modes). Mode is derived from `DOCKER_HOST` in the environment or `secrets.env`, applied only for the podman backend, modelled on `EnsureContainerRuntimeGroupMembership` in `agent/provision`. Rootful: a `podman.socket` drop-in with `SocketGroup=serviceloop`, `SocketMode=0660`, `DirectoryMode=0750`, then daemon-reload and restart the socket (root-equivalent access; document it). Rootless: subuid/subgid ranges for `serviceloop` (it is a `useradd --system` account and gets none), `newuidmap` present, `loginctl enable-linger serviceloop`, the user `podman.socket` enabled, and a `sparky-agent.service` drop-in relaxing `ProtectHome=true` for this mode only (it hides `/run/user`). Whether `ProtectHome=read-only` still allows connecting to the socket, and whether a lingering user manager works for a nologin system UID, must be verified, not assumed; the fallback is a system unit running `podman system service` as `User=serviceloop`. `check-gpu` already reproduces today's failure (an unprivileged user against the rootful socket gets "permission denied" and the socket hint).
2. Node-level GPU probe reported to the server, following the `report_interfaces`/`rescan_interfaces` pattern: new `gpu_check` message and `recheck_gpu` command in `internal/agentproto`, migration `000040` adding current-state columns on `nodes` (status, detail, checked_at) with SCHEMA.md updated in the same change, Nodes page badge plus an Admin Re-check button through the audit middleware. A failed probe WARNS ONLY; it never blocks a Load. Strictly decoded payloads mean agents must be upgraded before the server.
3. Full-stack run on the laptop as the packaged agent under `serviceloop`, in rootful then rootless mode: real server (`scripts/dev-server.sh`), a small model through Sparky's own download path (previously `RedHatAI/Qwen2-0.5B-Instruct-FP8`), a vLLM profile using the local `vllm/vllm-openai:latest` image, evidence of GPU utilization rising (use fixed-count single-shot `nvidia-smi --query-gpu` samples; the `-l`/`-lms` flags hung on a Spark), negative cases, and the Dead/health-resume checks on Podman. Commit the evidence script as `scripts/validate_gpu_passthrough.sh` (docker|podman). Installing the `.deb` creates the `serviceloop` and `sparky-peer` accounts, an sshd drop-in and a systemd unit; get the maintainer's go-ahead, and reverse everything afterward.
4. Docs sweep: correct the claims that the laptop "cannot pass a GPU to a container" ("permanent bare-metal case": PLANNING.md Decisions Log 2026-08-13 and the Dependencies and Blockers bullet, ARCHITECTURE.md Runtime Backends, SCHEMA.md Nodes `runtime_backend`, README.md); leave the historical CHANGELOG lines. The ARCHITECTURE.md manual-checklist item "CDI GPU passthrough verified on Podman" stays unchecked until item 3 is done.
5. Container cleanup: archive-before-remove and replace-on-launch are merged (#161, #162); the scheduled and manual cleanup, its settings row, the "system" audit actor and the UI are not built (see PLANNING.md Current Sprint / Active Work and the 2026-10-08 "container cleanup and log archive" Decisions Log entry). The probe label `sparky.probe` exists so a future sweep of `sparky.managed` containers cannot take a probe for an engine container.

## 12. Definition of done for THIS branch

- The Spark output has been received and interpreted per section 7.
- The matching records in section 8 are updated, tests and vet pass, and a PR is open for the maintainer with an honest verified / not-verified list.
- The Known Issues row for the Docker guard is closed only if the result genuinely supports it.
- You have stopped at the open PR and not merged it.
