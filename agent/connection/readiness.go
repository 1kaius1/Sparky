// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

// defaultInstanceStartupTimeout is used when Config.InstanceStartupTimeout
// is non-positive - see that field's own doc comment. Generous on purpose:
// this only matters for the rare "never comes up, never crashes" case,
// since waitForReady already returns as soon as a real completion succeeds
// (usually far sooner) or as soon as the process/container exits (also far
// sooner) - a legitimately large model loading from disk is the case this
// number has to accommodate.
const defaultInstanceStartupTimeout = 10 * time.Minute

// readinessPollInterval is how often waitForReady re-checks liveness/
// readiness while waiting.
const readinessPollInterval = 2 * time.Second

// readinessProbeHTTPTimeout bounds one readiness-probe HTTP call
// (models-list ping or the completion probe itself) - short, since a slow
// response here is itself evidence the engine isn't ready yet, not
// something worth waiting out within a single attempt.
const readinessProbeHTTPTimeout = 5 * time.Second

// healthCheckHTTPTimeout bounds one periodic health-check HTTP call - see
// sendInstanceHealth. Short for the same reason as
// readinessProbeHTTPTimeout: a slow response is itself the unhealthy
// signal.
const healthCheckHTTPTimeout = 5 * time.Second

// logsTailLines bounds how much diagnostic output waitForReady asks
// runtime.Backend.Logs for when building a failure message - enough to
// show a real error (a Python traceback, a CUDA OOM dump), not a full
// history.
const logsTailLines = 200

// errorMessageLogExcerptBytes bounds how much of Logs' output actually
// gets attached to a failed InstanceResult's error message - agentproto
// messages travel over the same WebSocket connection as everything else,
// so this stays a reasonable size regardless of how much a real container
// happened to log before dying.
const errorMessageLogExcerptBytes = 4096

// readinessProbePrompt is the fixed prompt sent to confirm an instance can
// genuinely generate, not just that its HTTP server answers - see
// waitForReady's own doc comment for why a real generation is checked
// structurally (a well-formed, non-empty, non-error response) rather than
// for any particular "known good" content: Sparky launches whatever model
// a profile names, so there is no way to know in advance what a "correct"
// answer to any prompt looks like for an arbitrary model.
const readinessProbePrompt = "Reply with a single word."

// engineProbe describes how to confirm an instance of a given engine type
// is genuinely ready to serve, and how to read its ongoing load signal -
// agent-side engine knowledge kept deliberately minimal and duplicated
// from internal/engines rather than imported, same reasoning as
// buildEngineLaunchArgs' own vllmDefaultImage constant: agent-side code
// never imports internal/'s server-side packages. vLLM, Aphrodite, and
// llama.cpp's server mode are all OpenAI-API-compatible, so every engine
// type shares one definition today - a future engine type with a
// different API shape would get its own entry instead of forcing a
// change here. llama.cpp specifically is unverified against real
// hardware - no llama.cpp launch has ever been tested through Sparky
// end to end (see PLANNING.md Known Issues) - so its entry is correct in
// principle, not empirically confirmed the way vLLM's is.
type engineProbe struct {
	// modelsPath is polled first, before any completion is attempted -
	// proves the HTTP API layer itself is up (the engine process is
	// alive and has bound its port) without spending a real generation
	// on a check that would just fail anyway if the server isn't even
	// listening yet.
	modelsPath string
	// chatCompletionsPath is where the real readiness probe (and,
	// implicitly, the model's own real usage) is served.
	chatCompletionsPath string
	// metricsPath is a best-effort Prometheus-format endpoint
	// sendInstanceHealth reads for the load-derived signal - empty means
	// this engine type has no known metrics endpoint (nothing is read,
	// InstanceHealth.Detail stays empty, matching Detail's own "not every
	// engine type reports this" framing).
	metricsPath string
}

var engineProbes = map[string]engineProbe{
	"vllm":      {modelsPath: "/v1/models", chatCompletionsPath: "/v1/chat/completions", metricsPath: "/metrics"},
	"aphrodite": {modelsPath: "/v1/models", chatCompletionsPath: "/v1/chat/completions", metricsPath: "/metrics"},
	"llamacpp":  {modelsPath: "/v1/models", chatCompletionsPath: "/v1/chat/completions", metricsPath: "/metrics"},
}

// activeInstance is what sendInstanceHealth needs to keep checking an
// instance runLoad has confirmed running - see Conn.activeInstances.
type activeInstance struct {
	Port       int
	ModelPath  string
	EngineType string
}

// trackActiveInstance records instanceID as confirmed running - called by
// runLoad only after waitForReady succeeds, never before, so
// sendInstanceHealth never polls an instance that hasn't itself been
// proven ready yet.
func (c *Conn) trackActiveInstance(instanceID string, port int, modelPath, engineType string) {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	c.activeInstances[instanceID] = activeInstance{Port: port, ModelPath: modelPath, EngineType: engineType}
}

// untrackActiveInstance stops sendInstanceHealth from checking instanceID
// - called by runUnload regardless of whether the stop itself succeeded.
func (c *Conn) untrackActiveInstance(instanceID string) {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	delete(c.activeInstances, instanceID)
}

// snapshotActiveInstances returns a point-in-time copy of every instance
// currently tracked - a copy, not the live map, so sendInstanceHealth can
// iterate it without holding activeMu for the whole (potentially slow,
// network-bound) duration of a health-check pass.
func (c *Conn) snapshotActiveInstances() map[string]activeInstance {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	snapshot := make(map[string]activeInstance, len(c.activeInstances))
	for id, inst := range c.activeInstances {
		snapshot[id] = inst
	}
	return snapshot
}

// waitForReady polls until instanceID is confirmed genuinely serving, or
// gives up - the fix for a real silent-success bug (see runLoad's own doc
// comment): runtime.Start succeeding only means the process/container
// itself launched, never that the engine inside it came up. Three
// possible outcomes:
//
//  1. The process/container exits before becoming ready - failure,
//     returned immediately rather than waiting out the rest of timeout,
//     since there is nothing left to wait for.
//  2. A real chat-completion probe succeeds (after the API layer itself
//     first responds to a cheap models-list check) - ready, returned as
//     soon as it happens rather than waiting out the rest of timeout.
//  3. Neither happens before timeout elapses - failure. A load that never
//     becomes reachable is exactly the silent-success bug this exists to
//     prevent, timeout or not - "I gave up waiting" is still an honest
//     failure to report, not a success to assume.
//
// engineType with no known engineProbes entry fails immediately, same
// "can't confirm it, can't claim it" reasoning - an unrecognized engine
// type has no defined way to prove readiness, so it can't be reported
// running either.
func (c *Conn) waitForReady(ctx context.Context, instanceID, engineType string, port int, modelPath string) error {
	timeout := c.cfg.InstanceStartupTimeout
	if timeout <= 0 {
		timeout = defaultInstanceStartupTimeout
	}

	probe, ok := engineProbes[engineType]
	if !ok {
		return fmt.Errorf("no readiness probe known for engine type %q", engineType)
	}

	client := &http.Client{Timeout: readinessProbeHTTPTimeout}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(readinessPollInterval)
	defer ticker.Stop()

	apiReachable := false
	for {
		if running, err := c.runtime.IsRunning(ctx, instanceID); err == nil && !running {
			return fmt.Errorf("process/container exited before becoming ready")
		}

		// Deliberately not else-if: an instance that's already up the
		// very first time either check runs (the common case for a
		// small/fast-loading model) proves readiness in one pass rather
		// than needlessly waiting a full readinessPollInterval between
		// "the API layer is up" and "a completion succeeds."
		if !apiReachable {
			apiReachable = httpGetOK(ctx, client, base+probe.modelsPath)
		}
		if apiReachable && completionProbeOK(ctx, client, base+probe.chatCompletionsPath, modelPath) {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("instance did not become ready within %s", timeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// httpGetOK reports whether a plain GET to url succeeds with a 2xx status
// - used for the cheap "is the API layer even up yet" check both
// waitForReady and sendInstanceHealth's reachability check need.
func httpGetOK(ctx context.Context, client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// completionProbeRequest/Response are the minimal OpenAI-compatible
// chat-completions shape needed to send readinessProbePrompt and confirm a
// real, well-formed answer came back - see completionProbeOK.
type completionProbeRequest struct {
	Model       string              `json:"model"`
	Messages    []map[string]string `json:"messages"`
	MaxTokens   int                 `json:"max_tokens"`
	Temperature float64             `json:"temperature"`
}

type completionProbeResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error json.RawMessage `json:"error"`
}

// completionProbeOK sends one real, minimal chat-completion request and
// reports whether a structurally valid answer came back - HTTP 200, no
// top-level "error" field, at least one choice with non-empty content.
// Deliberately not checking the content against any particular expected
// value - see readinessProbePrompt's own doc comment for why that can't
// generalize across arbitrary models. temperature: 0 and a small
// max_tokens keep this cheap: the goal is proving the forward pass works
// at all (catches a corrupted quantization, a first-request CUDA OOM),
// not exercising real generation quality.
func completionProbeOK(ctx context.Context, client *http.Client, url, modelPath string) bool {
	reqBody := completionProbeRequest{
		Model:       modelPath,
		Messages:    []map[string]string{{"role": "user", "content": readinessProbePrompt}},
		MaxTokens:   8,
		Temperature: 0,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}

	var probeResp completionProbeResponse
	if err := json.NewDecoder(resp.Body).Decode(&probeResp); err != nil {
		return false
	}
	if len(probeResp.Error) > 0 {
		return false
	}
	if len(probeResp.Choices) == 0 {
		return false
	}
	return strings.TrimSpace(probeResp.Choices[0].Message.Content) != ""
}

// readMetrics best-effort reads url (a Prometheus-format /metrics
// endpoint) and picks out a small, known set of vLLM/Aphrodite metric
// names - see agentproto.InstanceHealth.Detail's own doc comment for why
// this is deliberately not required for correctness: a metrics endpoint
// that's missing, unreachable, or exposes different metric names (e.g.
// llama.cpp, whose Prometheus metric names differ and are unverified
// against real hardware today) just means Detail stays empty, not a
// health-check failure. Prometheus's plain text exposition format is
// "<metric_name>{labels} <value>" per line - confirmed against a real
// vLLM instance's own /metrics output, which does carry a labels segment
// on these two metrics (`engine="0",model_name="..."`), not the bare
// "<metric_name> <value>" this function originally assumed before that
// real check caught it - so the metric name is matched on the prefix
// before any "{", ignoring the labels themselves entirely (this function
// has no need to distinguish per-engine/per-model values, only the
// aggregate reading). Anything else on the line (HELP/TYPE comments,
// unrelated metrics) is ignored.
func readMetrics(ctx context.Context, client *http.Client, url string) map[string]float64 {
	knownMetrics := map[string]string{
		"vllm:num_requests_running": "num_requests_running",
		"vllm:num_requests_waiting": "num_requests_waiting",
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}

	var detail map[string]float64
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// Strip any "{labels}" segment before matching - real vLLM output
		// carries one on these metrics (engine/model_name), confirmed
		// against a real instance.
		metricName := fields[0]
		if i := strings.IndexByte(metricName, '{'); i >= 0 {
			metricName = metricName[:i]
		}
		detailKey, known := knownMetrics[metricName]
		if !known {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		if detail == nil {
			detail = make(map[string]float64)
		}
		detail[detailKey] = value
	}
	return detail
}

// truncateForErrorMessage bounds logs to errorMessageLogExcerptBytes,
// keeping the tail (the most recent, most likely relevant output) rather
// than the head.
func truncateForErrorMessage(logs string) string {
	if len(logs) <= errorMessageLogExcerptBytes {
		return logs
	}
	return "..." + logs[len(logs)-errorMessageLogExcerptBytes:]
}

// sendInstanceHealth runs until ctx is canceled, checking every currently
// tracked active instance once per Config.InstanceHealthCheckInterval and
// reporting a healthy/unhealthy verdict - see agentproto.InstanceHealth's
// doc comment and SCHEMA.md Running instances' health_status/
// last_health_check_at, unpopulated by anything before this feature.
// Deliberately a cheap reachability check (GET the engine's own
// models-list endpoint) plus a best-effort read of its /metrics endpoint
// for the load-derived signal, not a repeated real completion the way
// waitForReady's one-time load-readiness check uses - a synthetic
// generation request every interval, forever, for every active instance,
// is real GPU-cycle overhead this periodic check has no need to pay once
// an instance has already proven at launch that it can generate.
func (c *Conn) sendInstanceHealth(ctx context.Context, conn *websocket.Conn) {
	interval := c.cfg.InstanceHealthCheckInterval
	if interval <= 0 {
		c.logger.Printf("agent connection: instance health checks disabled (non-positive InstanceHealthCheckInterval)")
		return
	}

	client := &http.Client{Timeout: healthCheckHTTPTimeout}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for instanceID, inst := range c.snapshotActiveInstances() {
				c.checkInstanceHealth(ctx, conn, client, instanceID, inst)
			}
		}
	}
}

// checkInstanceHealth performs and reports one instance's periodic health
// check - split out of sendInstanceHealth's loop body for readability.
func (c *Conn) checkInstanceHealth(ctx context.Context, conn *websocket.Conn, client *http.Client, instanceID string, inst activeInstance) {
	probe, ok := engineProbes[inst.EngineType]
	if !ok {
		// Same "can't check it, can't claim it" reasoning as
		// waitForReady - though in practice this can't happen, since
		// trackActiveInstance is only ever called after waitForReady
		// already resolved the same engine type successfully.
		return
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", inst.Port)

	status := agentproto.InstanceHealthStatusHealthy
	var detail map[string]float64
	if !httpGetOK(ctx, client, base+probe.modelsPath) {
		status = agentproto.InstanceHealthStatusUnhealthy
	} else if probe.metricsPath != "" {
		detail = readMetrics(ctx, client, base+probe.metricsPath)
	}

	env, err := agentproto.NewEnvelope(agentproto.TypeInstanceHealth, "", agentproto.InstanceHealth{
		InstanceID: instanceID,
		Status:     status,
		CheckedAt:  time.Now(),
		Detail:     detail,
	})
	if err != nil {
		c.logger.Printf("agent connection: build instance_health for %s: %v", instanceID, err)
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		c.logger.Printf("agent connection: marshal instance_health for %s: %v", instanceID, err)
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.logger.Printf("agent connection: send instance_health for %s: %v", instanceID, err)
	}
}
