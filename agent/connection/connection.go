// SPDX-License-Identifier: AGPL-3.0-or-later

// Package connection is sparky-agent's Connection goroutine - see
// docs/AGENT.md Service Architecture Notes: "owns the WebSocket
// lifecycle - dial, handshake with the bearer token, read loop, and
// reconnect-with-backoff on disconnect." Phase 5 of the agent runtime/
// WebSocket work (PLANNING.md) made the connection to
// internal/agentconn's server-side endpoint real; TypeStartTransfer
// (Model transfers) and TypeLoadInstance/TypeUnloadInstance (Running
// instances) are the real command types dispatch recognizes today.
package connection

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/agent/enginetransfer"
	"github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/agent/telemetry"
	"github.com/1kaius1/Sparky/agent/transfer"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// handshakeTimeout bounds both a single dial attempt and waiting for the
// hello_ack reply - matches internal/agentconn's own handshake timeout
// on the server side.
const handshakeTimeout = 10 * time.Second

// heartbeatInterval is how often Conn sends a keepalive heartbeat over an
// established connection - see agentproto.Heartbeat's doc comment: this
// is distinct from telemetry (a separate goroutine, on its own
// operator-configurable interval - see Config.TelemetryPollInterval). No
// SPARKY_* env var controls the heartbeat interval - docs/AGENT.md
// doesn't define one, and there's no case yet for making it
// operator-tunable.
const heartbeatInterval = 30 * time.Second

const (
	defaultMinBackoff = 1 * time.Second
	defaultMaxBackoff = 30 * time.Second
)

// runtimeBackend is runtime.Backend, aliased locally so this package's own
// doc comments and test fakes read as this package's concern - the
// concrete implementation (agent/runtime/containers or
// agent/runtime/baremetal) is chosen once at startup by cmd/sparky-agent
// based on the node's configured runtime_backend (SCHEMA.md Nodes), and
// this package never branches on which one it has.
type runtimeBackend = runtime.Backend

// transferExecutor is the subset of *transfer.Executor this package
// needs - narrow enough to fake in tests without a real HTTP download.
type transferExecutor interface {
	Download(ctx context.Context, modelRef, quantization, destDir string, progress transfer.ProgressFunc) error
}

// engineTransferExecutor is the subset of *enginetransfer.Executor this
// package needs - narrow enough to fake in tests without a real HTTP
// download or `tar` shell-out. Separate from transferExecutor rather than
// unified into one interface - the two download genuinely different things
// (a Hugging Face model repository vs. a single checksum-verified,
// versioned-install release tarball) with different return shapes, see
// PLANNING.md's 2026-08-15 Decisions Log entry.
type engineTransferExecutor interface {
	Provision(ctx context.Context, engineType, version, installRoot string, progress enginetransfer.ProgressFunc) (installPath string, installedSizeBytes int64, err error)
}

// telemetryCollector is the subset of *telemetry.Collector this package
// needs - narrow enough to fake in tests without shelling out to
// nvidia-smi or reading the real /proc.
type telemetryCollector interface {
	Read(ctx context.Context) (telemetry.Reading, error)
}

// Config is what Conn needs to dial and authenticate - the subset of
// agent/config.Config relevant to this package, so it doesn't depend on
// the whole agent's environment surface.
type Config struct {
	CentralURL  string
	BearerToken string
	NodeName    string

	// ModelStoragePath is where a TypeStartTransfer download lands -
	// SPARKY_MODEL_STORAGE_PATH, per docs/AGENT.md Configuration. Not
	// defaulted here or anywhere in this package - CLAUDE.md's rule
	// against hardcoding platform-specific paths applies to this value
	// like any other, so an empty one is passed straight through to
	// filepath.Join rather than silently substituted.
	ModelStoragePath string

	// EngineBinaryPaths maps an engine type ("vllm" / "llamacpp" - the
	// same string values as internal/db.ProfileEngineType) to the local
	// executable to run for it, on nodes configured for the bare-metal
	// runtime backend - SPARKY_LLAMACPP_BINARY_PATH/SPARKY_VLLM_BINARY_PATH,
	// per docs/AGENT.md Configuration. Unused by the containers backend.
	// An engine type absent from this map (or mapped to "") means this
	// node doesn't run that engine - a load_instance for it fails clearly
	// via agent/runtime/baremetal.Backend.Start's own error, reported back
	// as a failed InstanceResult like any other real launch failure.
	EngineBinaryPaths map[string]string

	// EngineInstallPath is the root directory a TypeStartEngineTransfer
	// provisioning run installs into - SPARKY_ENGINE_INSTALL_PATH, per
	// docs/AGENT.md Configuration. Same not-defaulted-here treatment as
	// ModelStoragePath. Unused by the containers backend, which gets engine
	// software via container images, not this mechanism.
	EngineInstallPath string

	// RuntimeBackend is SPARKY_RUNTIME_BACKEND ("bare-metal" / "docker" /
	// "podman", per docs/AGENT.md Configuration) - agent/config.Config's
	// own copy of the same value, already used once at startup to decide
	// which runtime.Backend implementation to construct. Threaded through
	// separately here because buildEngineLaunchArgs needs it too: a vLLM
	// load_instance on bare-metal execs the raw vllm binary directly and
	// must prepend its "serve" subcommand itself, while the containers
	// backend's vllm/vllm-openai image already bakes "serve" into its own
	// ENTRYPOINT (confirmed via a real `podman inspect` - PLANNING.md
	// Decisions Log) - so this same launch-arg logic needs to answer
	// differently per backend, not just per engine type.
	RuntimeBackend string

	// TelemetryPollInterval is how often the telemetry goroutine takes
	// and pushes a reading - SPARKY_TELEMETRY_POLL_INTERVAL, per
	// docs/AGENT.md Configuration. Parsed by cmd/sparky-agent from
	// agent/config.Config's string form, which fails fast on an
	// unparseable duration - but a zero value parses successfully
	// (e.g. "0s"), so sendTelemetry still guards against a non-positive
	// value itself rather than trusting the caller blindly: a
	// time.NewTicker panic here would be unrecovered and take the whole
	// agent process down over what should only ever disable telemetry.
	TelemetryPollInterval time.Duration
}

// Conn owns the agent's single persistent WebSocket connection to the
// central app.
type Conn struct {
	cfg            Config
	runtime        runtimeBackend
	transfer       transferExecutor
	engineTransfer engineTransferExecutor
	telemetry      telemetryCollector
	logger         *log.Logger

	minBackoff time.Duration
	maxBackoff time.Duration

	// transferWG tracks in-flight transfer goroutines so Run can wait for
	// them to reach a safe stopping point on shutdown rather than the
	// process exiting mid-write - see docs/AGENT.md Service Architecture
	// Notes.
	transferWG sync.WaitGroup

	// engineTransferWG tracks in-flight start_engine_transfer goroutines -
	// same reasoning as transferWG, kept as its own group rather than
	// sharing transferWG since a model-weight download and an engine
	// provisioning run are unrelated operations with no reason to block
	// each other's shutdown wait, matching the existing transferWG/
	// instanceWG split.
	engineTransferWG sync.WaitGroup

	// instanceWG tracks in-flight load_instance/unload_instance
	// goroutines - same reasoning as transferWG, kept as a separate group
	// since a load and a transfer are unrelated operations with no reason
	// to block each other's shutdown wait.
	instanceWG sync.WaitGroup
}

// New constructs a Conn.
func New(cfg Config, runtime runtimeBackend, transferExec transferExecutor, engineTransferExec engineTransferExecutor, collector telemetryCollector, logger *log.Logger) *Conn {
	return &Conn{
		cfg:            cfg,
		runtime:        runtime,
		transfer:       transferExec,
		engineTransfer: engineTransferExec,
		telemetry:      collector,
		logger:         logger,
		minBackoff:     defaultMinBackoff,
		maxBackoff:     defaultMaxBackoff,
	}
}

// Run blocks until ctx is canceled, maintaining the connection: dial,
// handshake, read loop, and - on any failure or disconnect - reconnect
// after an exponential backoff (reset to the minimum after any
// connection that got far enough to complete its handshake). Every
// outbound message this agent sends goes over the connection this
// method owns, per docs/AGENT.md: "Every other goroutine sends outbound
// messages through this one rather than managing the socket itself" -
// heartbeats and transfer progress are the two senders that do today.
//
// Before returning, Run waits for every in-flight transfer goroutine
// (dispatch's TypeStartTransfer case) and load/unload goroutine
// (TypeLoadInstance/TypeUnloadInstance) to reach a safe stopping point -
// see docs/AGENT.md Service Architecture Notes: "sync.WaitGroup tracks
// in-flight transfers so graceful shutdown can wait for them ... rather
// than killing a transfer mid-write," and Signal Handling's "stop managed
// engine processes cleanly." Each such goroutine is spawned with this
// method's ctx, not a per-connection one, so it keeps running (and gets a
// chance to finish or fail cleanly) across a mere WebSocket reconnect -
// only this method's own cancellation (agent shutdown) stops it.
//
// c.runtime.Shutdown runs after instanceWG.Wait (so no in-flight runLoad
// can still be starting a process while Shutdown is stopping everything)
// and before transferWG.Wait (unrelated - order between the two doesn't
// matter, kept last to preserve this method's existing structure). A
// no-op for the containers backend; for bare-metal, this is what stops
// every still-running exec'd engine process on a clean agent exit - see
// docs/AGENT.md Signal Handling.
func (c *Conn) Run(ctx context.Context) {
	defer c.transferWG.Wait()
	defer c.engineTransferWG.Wait()
	defer c.shutdownRuntime()
	defer c.instanceWG.Wait()

	backoff := c.minBackoff
	for ctx.Err() == nil {
		connected, err := c.runOnce(ctx)
		if err != nil && ctx.Err() == nil {
			c.logger.Printf("agent connection: %v", err)
		}
		if connected {
			backoff = c.minBackoff
		}
		if ctx.Err() != nil {
			return
		}

		wait := jitter(backoff)
		c.logger.Printf("agent connection: reconnecting in %s", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		backoff *= 2
		if backoff > c.maxBackoff {
			backoff = c.maxBackoff
		}
	}
}

// runtimeShutdownTimeout bounds how long Run's exit path waits for
// c.runtime.Shutdown - generous headroom under the systemd unit's default
// TimeoutStopSec (90s), matching agent/runtime/baremetal's own
// stopGracePeriod reasoning.
const runtimeShutdownTimeout = 30 * time.Second

// shutdownRuntime stops whatever this Conn's runtime backend is still
// tracking, as Run exits - see Run's own doc comment for why this fits
// between instanceWG.Wait and transferWG.Wait in the defer order. Any
// error is logged, not returned - Run's shutdown path has no caller left
// to hand an error back to.
func (c *Conn) shutdownRuntime() {
	ctx, cancel := context.WithTimeout(context.Background(), runtimeShutdownTimeout)
	defer cancel()
	if err := c.runtime.Shutdown(ctx); err != nil {
		c.logger.Printf("agent connection: runtime shutdown: %v", err)
	}
}

// runOnce dials once, completes the handshake, and then blocks in the
// read loop until the connection ends. connected reports whether the
// handshake was reached and accepted - Run uses it to decide whether to
// reset the backoff, distinguishing "never got connected" from "was
// connected, then dropped."
func (c *Conn) runOnce(ctx context.Context) (connected bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	conn, _, err := websocket.Dial(dialCtx, c.cfg.CentralURL, nil)
	cancel()
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()

	if err := c.handshake(ctx, conn); err != nil {
		conn.Close(websocket.StatusPolicyViolation, "handshake failed")
		return false, fmt.Errorf("handshake: %w", err)
	}
	c.logger.Printf("agent connection: connected to %s as %q", c.cfg.CentralURL, c.cfg.NodeName)

	readCtx, cancelBackgroundSenders := context.WithCancel(ctx)
	defer cancelBackgroundSenders()
	go c.sendHeartbeats(readCtx, conn)
	go c.sendTelemetry(readCtx, conn)

	return true, c.readLoop(ctx, conn)
}

// handshake sends TypeHello and waits for an accepting TypeHelloAck -
// see internal/agentconn's server-side handshake, which this mirrors.
func (c *Conn) handshake(ctx context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	env, err := agentproto.NewEnvelope(agentproto.TypeHello, "", agentproto.Hello{
		NodeName:    c.cfg.NodeName,
		BearerToken: c.cfg.BearerToken,
	})
	if err != nil {
		return fmt.Errorf("build hello: %w", err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal hello: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	_, respRaw, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read hello_ack: %w", err)
	}
	var respEnv agentproto.Envelope
	if err := json.Unmarshal(respRaw, &respEnv); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	if respEnv.Type != agentproto.TypeHelloAck {
		return fmt.Errorf("first response type = %q, want %q", respEnv.Type, agentproto.TypeHelloAck)
	}
	var ack agentproto.HelloAck
	if err := respEnv.DecodePayload(&ack); err != nil {
		return fmt.Errorf("decode hello_ack payload: %w", err)
	}
	if !ack.Accepted {
		return fmt.Errorf("central app rejected connection: %s", ack.Reason)
	}
	return nil
}

// sendHeartbeats runs until ctx is canceled, sending a heartbeat every
// heartbeatInterval. A write failure is left for the read loop to
// notice and act on (it's reading the same connection concurrently) -
// this goroutine just stops.
func (c *Conn) sendHeartbeats(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			env, err := agentproto.NewEnvelope(agentproto.TypeHeartbeat, "", agentproto.Heartbeat{SentAt: time.Now()})
			if err != nil {
				c.logger.Printf("agent connection: build heartbeat: %v", err)
				continue
			}
			raw, err := json.Marshal(env)
			if err != nil {
				c.logger.Printf("agent connection: marshal heartbeat: %v", err)
				continue
			}
			if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
				return
			}
		}
	}
}

// sendTelemetry runs until ctx is canceled, taking one hardware reading
// every Config.TelemetryPollInterval and pushing it as telemetry - see
// docs/AGENT.md Service Architecture Notes' Telemetry goroutine: "does
// not wait on the command loop." A read or send failure is logged and
// that tick is skipped, same reasoning as sendHeartbeats' write-failure
// handling - one missed reading isn't worth tearing down the connection
// over, and the next tick tries again.
func (c *Conn) sendTelemetry(ctx context.Context, conn *websocket.Conn) {
	// time.NewTicker panics on a non-positive interval - guarded here
	// rather than trusted blindly from Config, since a goroutine panic is
	// unrecovered and takes the whole agent process down over what should
	// only ever disable telemetry for this connection's lifetime.
	if c.cfg.TelemetryPollInterval <= 0 {
		c.logger.Printf("agent connection: telemetry disabled (non-positive TelemetryPollInterval)")
		return
	}

	ticker := time.NewTicker(c.cfg.TelemetryPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reading, err := c.telemetry.Read(ctx)
			if err != nil {
				c.logger.Printf("agent connection: read telemetry: %v", err)
				continue
			}

			env, err := agentproto.NewEnvelope(agentproto.TypeTelemetry, "", agentproto.Telemetry{
				RecordedAt:          time.Now(),
				GPUUtilizationPct:   reading.GPUUtilizationPct,
				GPUMemoryUsedMB:     reading.GPUMemoryUsedMB,
				GPUMemoryTotalMB:    reading.GPUMemoryTotalMB,
				CPUUtilizationPct:   reading.CPUUtilizationPct,
				SystemMemoryUsedMB:  reading.SystemMemoryUsedMB,
				SystemMemoryTotalMB: reading.SystemMemoryTotalMB,
			})
			if err != nil {
				c.logger.Printf("agent connection: build telemetry: %v", err)
				continue
			}
			raw, err := json.Marshal(env)
			if err != nil {
				c.logger.Printf("agent connection: marshal telemetry: %v", err)
				continue
			}
			if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
				return
			}
		}
	}
}

// readLoop blocks reading messages until the connection errors or ctx is
// canceled (canceling ctx aborts an in-progress Read and closes the
// connection - this is how Run's shutdown reaches an active connection;
// see docs/AGENT.md Signal Handling. This is not a graceful WebSocket
// close handshake, just an abrupt close via context cancellation - the
// central app's connection lifecycle handling (internal/agentconn)
// treats any read failure as a disconnect regardless of how it happened,
// so this is sufficient without the extra complexity of racing a close
// handshake against further reads).
func (c *Conn) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var env agentproto.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			c.logger.Printf("agent connection: received malformed message: %v", err)
			continue
		}
		c.dispatch(ctx, conn, env)
	}
}

// dispatch routes a received message. c.runtime is already wired in,
// ready for a real command type to call into - see runtimeBackend's doc
// comment for why there isn't one yet.
func (c *Conn) dispatch(ctx context.Context, conn *websocket.Conn, env agentproto.Envelope) {
	switch env.Type {
	case agentproto.TypeHeartbeat:
		// A heartbeat from the central app is itself just a keepalive -
		// no action beyond having read it.
	case agentproto.TypeError:
		var errPayload agentproto.ErrorPayload
		if err := env.DecodePayload(&errPayload); err != nil {
			c.logger.Printf("agent connection: received malformed error payload: %v", err)
			return
		}
		c.logger.Printf("agent connection: central app reported an error: %s (code %s)", errPayload.Message, errPayload.Code)
	case agentproto.TypeStartTransfer:
		var start agentproto.StartTransfer
		if err := env.DecodePayload(&start); err != nil {
			c.logger.Printf("agent connection: received malformed start_transfer payload: %v", err)
			return
		}
		c.transferWG.Add(1)
		go func() {
			defer c.transferWG.Done()
			c.runTransfer(ctx, conn, start)
		}()
	case agentproto.TypeStartEngineTransfer:
		var start agentproto.StartEngineTransfer
		if err := env.DecodePayload(&start); err != nil {
			c.logger.Printf("agent connection: received malformed start_engine_transfer payload: %v", err)
			return
		}
		c.engineTransferWG.Add(1)
		go func() {
			defer c.engineTransferWG.Done()
			c.runEngineTransfer(ctx, conn, start)
		}()
	case agentproto.TypeLoadInstance:
		var load agentproto.LoadInstance
		if err := env.DecodePayload(&load); err != nil {
			c.logger.Printf("agent connection: received malformed load_instance payload: %v", err)
			return
		}
		c.instanceWG.Add(1)
		go func() {
			defer c.instanceWG.Done()
			c.runLoad(ctx, conn, load)
		}()
	case agentproto.TypeUnloadInstance:
		var unload agentproto.UnloadInstance
		if err := env.DecodePayload(&unload); err != nil {
			c.logger.Printf("agent connection: received malformed unload_instance payload: %v", err)
			return
		}
		c.instanceWG.Add(1)
		go func() {
			defer c.instanceWG.Done()
			c.runUnload(ctx, conn, unload)
		}()
	case agentproto.TypeCheckInstance:
		var check agentproto.CheckInstance
		if err := env.DecodePayload(&check); err != nil {
			c.logger.Printf("agent connection: received malformed check_instance payload: %v", err)
			return
		}
		c.instanceWG.Add(1)
		go func() {
			defer c.instanceWG.Done()
			c.runCheckInstance(ctx, conn, check)
		}()
	default:
		c.logger.Printf("agent connection: received unhandled message type %q", env.Type)
	}
}

// runTransfer runs one transfer to completion - one goroutine per active
// transfer, per docs/AGENT.md Service Architecture Notes, so a
// long-running download never blocks readLoop's command handling.
// Progress is pushed back to the central app as TypeTransferProgress
// messages via c.transfer's throttled callback (agent/transfer.Executor's
// ProgressFunc); c.transfer.Download itself already reports a final
// StatusCompleted/StatusFailed call, so the only thing left to do with a
// returned error here is log it for local operator visibility
// (journalctl).
//
// conn is the connection this transfer was dispatched on. ctx is this
// method's caller's ctx (ultimately Run's) - see Run's doc comment for
// why a transfer keeps running across a mere WebSocket reconnect rather
// than being tied to the connection that happened to dispatch it: if conn
// drops mid-transfer, progress pushes over it start failing silently
// (logged, not fatal to the download) until the transfer finishes on its
// own timeline - a known, accepted v0.1.0 gap, since nothing yet
// redirects an in-flight transfer's progress reporting to a newer
// connection after a reconnect.
func (c *Conn) runTransfer(ctx context.Context, conn *websocket.Conn, start agentproto.StartTransfer) {
	destDir := filepath.Join(c.cfg.ModelStoragePath, filepath.FromSlash(start.ModelRef))

	progress := func(bytesTransferred, bytesTotal int64, status, errMsg string) {
		env, err := agentproto.NewEnvelope(agentproto.TypeTransferProgress, "", agentproto.TransferProgress{
			TransferID:       start.TransferID,
			BytesTransferred: bytesTransferred,
			BytesTotal:       bytesTotal,
			Status:           status,
			ErrorMessage:     errMsg,
		})
		if err != nil {
			c.logger.Printf("agent connection: build transfer_progress for %s: %v", start.TransferID, err)
			return
		}
		raw, err := json.Marshal(env)
		if err != nil {
			c.logger.Printf("agent connection: marshal transfer_progress for %s: %v", start.TransferID, err)
			return
		}
		// conn.Write is safe for concurrent use (coder/websocket - see
		// internal/agentconn.Registry.Send's doc comment for the same
		// claim, confirmed against the library itself) - sendHeartbeats
		// may be writing to this same connection concurrently.
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			c.logger.Printf("agent connection: send transfer_progress for %s: %v", start.TransferID, err)
		}
	}

	if err := c.transfer.Download(ctx, start.ModelRef, start.Quantization, destDir, progress); err != nil {
		c.logger.Printf("agent connection: transfer %s failed: %v", start.TransferID, err)
	}
}

// runEngineTransfer runs one engine provisioning run to completion - one
// goroutine per active run, same reasoning as runTransfer. Progress is
// pushed back to the central app as TypeEngineTransferProgress messages via
// c.engineTransfer's throttled callback (agent/enginetransfer.Executor's
// ProgressFunc); c.engineTransfer.Provision itself already reports a final
// StatusCompleted/StatusFailed call, so the only thing left to do with a
// returned error here is log it for local operator visibility
// (journalctl) - same shape as runTransfer's own error handling.
//
// conn and ctx follow the identical reasoning documented on runTransfer:
// ctx is Run's own long-lived context, so a provisioning run keeps going
// (and gets a chance to finish or fail cleanly) across a mere WebSocket
// reconnect, at the cost of the same known gap - progress pushes over a
// stale conn fail silently (logged) until the run finishes on its own
// timeline.
func (c *Conn) runEngineTransfer(ctx context.Context, conn *websocket.Conn, start agentproto.StartEngineTransfer) {
	progress := func(p enginetransfer.Progress) {
		env, err := agentproto.NewEnvelope(agentproto.TypeEngineTransferProgress, "", agentproto.EngineTransferProgress{
			TransferID:         start.TransferID,
			BytesTransferred:   p.BytesTransferred,
			BytesTotal:         p.BytesTotal,
			Status:             p.Status,
			ErrorMessage:       p.ErrorMessage,
			InstallPath:        p.InstallPath,
			InstalledSizeBytes: p.InstalledSizeBytes,
		})
		if err != nil {
			c.logger.Printf("agent connection: build engine_transfer_progress for %s: %v", start.TransferID, err)
			return
		}
		raw, err := json.Marshal(env)
		if err != nil {
			c.logger.Printf("agent connection: marshal engine_transfer_progress for %s: %v", start.TransferID, err)
			return
		}
		// conn.Write is safe for concurrent use - see runTransfer's progress
		// closure above for the same claim and its source.
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			c.logger.Printf("agent connection: send engine_transfer_progress for %s: %v", start.TransferID, err)
		}
	}

	if _, _, err := c.engineTransfer.Provision(ctx, start.EngineType, start.Version, c.cfg.EngineInstallPath, progress); err != nil {
		c.logger.Printf("agent connection: engine transfer %s failed: %v", start.TransferID, err)
	}
}

// resolveModelPath locates a model already downloaded to local storage for
// a load_instance command - the same destDir a start_transfer download for
// the same ModelRef landed in (see runTransfer above). A full-GPU-residency
// engine (vLLM-style) is pointed at that directory itself, since it expects
// a whole HF Transformers-format directory (config, tokenizer, every
// safetensors shard); a partial-offload engine (llama.cpp-style) is
// pointed at a single .gguf file within it. When quantization is empty,
// this preserves the original v0.1.0 behavior exactly: require exactly
// one .gguf file present, erroring on ambiguity (correct for a
// single-quantization repo; still fails clearly for a not-yet-pinned
// multi-quantization one, same as before). When quantization is
// non-empty, it resolves directly to the one file matching it - no
// exactly-one requirement, since the value already disambiguates.
func (c *Conn) resolveModelPath(modelRef, quantization string, requiresFullGPUResidency bool) (string, error) {
	dir := filepath.Join(c.cfg.ModelStoragePath, filepath.FromSlash(modelRef))
	if requiresFullGPUResidency {
		return dir, nil
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.gguf"))
	if err != nil {
		return "", fmt.Errorf("glob for a .gguf file in %s: %w", dir, err)
	}

	if quantization != "" {
		var filtered []string
		for _, m := range matches {
			if strings.Contains(filepath.Base(m), quantization) {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) != 1 {
			return "", fmt.Errorf("expected exactly one .gguf file matching quantization %q in %s, found %d", quantization, dir, len(filtered))
		}
		return filtered[0], nil
	}

	if len(matches) != 1 {
		return "", fmt.Errorf("expected exactly one .gguf file in %s, found %d", dir, len(matches))
	}
	return matches[0], nil
}

// resolveEngineBinaryPath returns the local binary path to launch for
// engineType, pinning to a specific installed version if version (from
// LoadInstance.EngineVersion) is set - see SCHEMA.md Node engine
// inventory and PLANNING.md's per-profile engine version pinning entry.
//
// Unpinned (version == "") returns exactly what EngineBinaryPaths already
// resolves to today - zero behavior change for any profile that doesn't
// set engine_version. A pinned version is resolved by reusing the
// *filename* of the operator's static SPARKY_<ENGINE>_BINARY_PATH config
// (which already points through the `latest` symlink, e.g.
// .../llamacpp/latest/llama-server - see docs/AGENT.md Engine binary
// provisioning) under the specific version's own directory instead:
// SPARKY_ENGINE_INSTALL_PATH/<engineType>/<version>/<same filename> - so
// no new per-engine binary-name configuration is needed. If either
// EngineBinaryPaths or EngineInstallPath is unset, this degrades to
// today's flat lookup and lets agent/runtime/baremetal.Backend.Start's
// existing "no local binary configured" error fire the same way it
// always has; a pinned version that isn't actually installed similarly
// surfaces as Start's own "no such file or directory" failure, reported
// back as a failed InstanceResult like any other launch failure -
// deliberately not pre-validated against node_engine_inventory (confirmed
// with the user - a bad pin fails clearly at launch time, matching
// required_memory_gb's own "attempt and report failure" precedent).
func (c *Conn) resolveEngineBinaryPath(engineType, version string) string {
	configured := c.cfg.EngineBinaryPaths[engineType]
	if version == "" || configured == "" || c.cfg.EngineInstallPath == "" {
		return configured
	}
	return filepath.Join(c.cfg.EngineInstallPath, engineType, version, filepath.Base(configured))
}

// buildEngineLaunchArgs assembles the argv Sparky passes to the engine. The
// --model/--port/--host flag form is confirmed valid vllm serve syntax
// (PLANNING.md Decisions Log: a real `vllm serve --help=ModelConfig`
// documents --model as a real flag, not just the positional model_tag its
// usage synopsis shows) - so this same flag shape already works for both
// llama.cpp and vLLM. "serve" itself is only prepended for the bare-metal
// backend running a vLLM profile: bare-metal execs the raw vllm binary
// directly (agent/runtime/baremetal.Backend.Start), which has no entrypoint
// to supply the subcommand, so `vllm --model ...` with no subcommand is
// invalid syntax. The containers backend needs no such prepending - a real
// `podman inspect` against docker.io/vllm/vllm-openai:latest (PLANNING.md
// Decisions Log) confirmed its ENTRYPOINT already is ["vllm","serve"], and
// Docker/Podman appends spec.Args after a container's ENTRYPOINT, so
// prepending "serve" there would double it ("vllm serve serve ...",
// invalid).
func buildEngineLaunchArgs(runtimeBackend, engineType, modelPath string, port int, extra []string) []string {
	args := []string{"--model", modelPath, "--port", strconv.Itoa(port), "--host", "0.0.0.0"}
	if engineType == "vllm" && runtimeBackend == "bare-metal" {
		args = append([]string{"serve"}, args...)
	}
	return append(args, extra...)
}

// runLoad starts a Running instance - one goroutine per load/unload
// command, same reasoning as runTransfer, so a slow image pull or process
// start never blocks readLoop's command handling. The outcome is always
// reported back as an instance_result message, success or failure - never
// silently dropped, since the central app has no other way to learn what
// actually happened on this node.
func (c *Conn) runLoad(ctx context.Context, conn *websocket.Conn, load agentproto.LoadInstance) {
	modelPath, err := c.resolveModelPath(load.ModelRef, load.Quantization, load.RequiresFullGPUResidency)
	if err != nil {
		c.logger.Printf("agent connection: resolve model path for instance %s: %v", load.InstanceID, err)
		c.sendInstanceResult(ctx, conn, load.InstanceID, agentproto.InstanceStatusFailed, 0, err.Error())
		return
	}

	args := buildEngineLaunchArgs(c.cfg.RuntimeBackend, load.EngineType, modelPath, load.Port, load.Args)
	spec := runtime.Spec{
		InstanceID: load.InstanceID,
		EngineType: load.EngineType,
		Image:      load.Image,
		BinaryPath: c.resolveEngineBinaryPath(load.EngineType, load.EngineVersion),
		Args:       args,
		Port:       load.Port,
		// Read-only: the agent already owns writing to this directory
		// (runTransfer above) - the containers backend's engine only ever
		// needs to read the model files back out. Mounted at the identical
		// path inside the container as on the host, so modelPath (resolved
		// above from this same host path) needs no translation to also
		// resolve inside the container. Unused by the bare-metal backend,
		// which already has direct filesystem access.
		Mounts: []string{c.cfg.ModelStoragePath + ":" + c.cfg.ModelStoragePath + ":ro"},
	}

	if _, err := c.runtime.Start(ctx, spec); err != nil {
		c.logger.Printf("agent connection: start instance %s: %v", load.InstanceID, err)
		c.sendInstanceResult(ctx, conn, load.InstanceID, agentproto.InstanceStatusFailed, 0, err.Error())
		return
	}
	c.sendInstanceResult(ctx, conn, load.InstanceID, agentproto.InstanceStatusRunning, load.Port, "")
}

// runUnload stops a Running instance, identified by InstanceID - each
// backend derives its own internal identity from it (see
// containers.InstanceContainerName and agent/runtime/baremetal.Backend's
// own tracking map).
func (c *Conn) runUnload(ctx context.Context, conn *websocket.Conn, unload agentproto.UnloadInstance) {
	if err := c.runtime.Stop(ctx, unload.InstanceID); err != nil {
		c.logger.Printf("agent connection: stop instance %s: %v", unload.InstanceID, err)
		c.sendInstanceResult(ctx, conn, unload.InstanceID, agentproto.InstanceStatusFailed, 0, err.Error())
		return
	}
	c.sendInstanceResult(ctx, conn, unload.InstanceID, agentproto.InstanceStatusStopped, 0, "")
}

// runCheckInstance answers the central app's running_instances staleness
// reconciliation sweep (PLANNING.md's Decisions Log) - triggered after a
// fresh connection, once per non-terminal instance the central app still
// believes is running on this node. Reports back via the same
// TypeInstanceResult mechanism runLoad/runUnload use, since "here's this
// instance's current status" is exactly what that message already carries
// - no new response type needed.
//
// An IsRunning error (e.g. a transient Docker daemon hiccup) deliberately
// sends nothing back - "I don't know" and "it's stopped" are different
// things, and a transient infrastructure error must not falsely mark a
// row stopped that might still be perfectly fine; the central app's copy
// of running_instances is left exactly as it was, to be re-checked on a
// future reconnect.
func (c *Conn) runCheckInstance(ctx context.Context, conn *websocket.Conn, check agentproto.CheckInstance) {
	running, err := c.runtime.IsRunning(ctx, check.InstanceID)
	if err != nil {
		c.logger.Printf("agent connection: check instance %s: %v", check.InstanceID, err)
		return
	}

	status := agentproto.InstanceStatusStopped
	if running {
		status = agentproto.InstanceStatusRunning
	}
	c.sendInstanceResult(ctx, conn, check.InstanceID, status, 0, "")
}

// sendInstanceResult reports a load/unload outcome back to the central
// app - see agentproto.InstanceResult's doc comment.
func (c *Conn) sendInstanceResult(ctx context.Context, conn *websocket.Conn, instanceID, status string, actualPort int, errMsg string) {
	env, err := agentproto.NewEnvelope(agentproto.TypeInstanceResult, "", agentproto.InstanceResult{
		InstanceID:   instanceID,
		Status:       status,
		ActualPort:   actualPort,
		ErrorMessage: errMsg,
	})
	if err != nil {
		c.logger.Printf("agent connection: build instance_result for %s: %v", instanceID, err)
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		c.logger.Printf("agent connection: marshal instance_result for %s: %v", instanceID, err)
		return
	}
	// conn.Write is safe for concurrent use - see runTransfer's progress
	// closure above for the same claim and its source.
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.logger.Printf("agent connection: send instance_result for %s: %v", instanceID, err)
	}
}

// jitter returns a duration in [d/2, d) - "equal jitter" - so many
// agents reconnecting after the same event (e.g. a central app restart)
// don't all retry in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)))
}
