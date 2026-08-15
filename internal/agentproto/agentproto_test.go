// SPDX-License-Identifier: AGPL-3.0-or-later

package agentproto

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestEnvelope_RoundTrip_Hello(t *testing.T) {
	want := Hello{NodeName: "spark-01", BearerToken: "s3cr3t"}

	env, err := NewEnvelope(TypeHello, "req-1", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal(Envelope) error: %v", err)
	}

	var decodedEnv Envelope
	if err := json.Unmarshal(raw, &decodedEnv); err != nil {
		t.Fatalf("Unmarshal(Envelope) error: %v", err)
	}
	if decodedEnv.Type != TypeHello {
		t.Errorf("Type = %q, want %q", decodedEnv.Type, TypeHello)
	}
	if decodedEnv.RequestID != "req-1" {
		t.Errorf("RequestID = %q, want %q", decodedEnv.RequestID, "req-1")
	}

	var got Hello
	if err := decodedEnv.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("Hello = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_HelloAck(t *testing.T) {
	want := HelloAck{Accepted: false, Reason: "unknown node"}

	env, err := NewEnvelope(TypeHelloAck, "req-1", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got HelloAck
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("HelloAck = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_Heartbeat(t *testing.T) {
	want := Heartbeat{SentAt: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}

	env, err := NewEnvelope(TypeHeartbeat, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}
	if env.RequestID != "" {
		t.Errorf("RequestID = %q, want empty for an unsolicited heartbeat", env.RequestID)
	}

	var got Heartbeat
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if !got.SentAt.Equal(want.SentAt) {
		t.Errorf("SentAt = %v, want %v", got.SentAt, want.SentAt)
	}
}

func TestEnvelope_RoundTrip_ErrorPayload(t *testing.T) {
	want := ErrorPayload{Message: "malformed message", Code: "BAD_REQUEST"}

	env, err := NewEnvelope(TypeError, "req-2", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got ErrorPayload
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("ErrorPayload = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_StartTransfer(t *testing.T) {
	want := StartTransfer{TransferID: "xfer-1", ModelRef: "meta-llama/Llama-3-8B"}

	env, err := NewEnvelope(TypeStartTransfer, "req-3", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got StartTransfer
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("StartTransfer = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_TransferProgress(t *testing.T) {
	want := TransferProgress{
		TransferID:       "xfer-1",
		BytesTransferred: 4096,
		BytesTotal:       8192,
		Status:           "transferring",
	}

	env, err := NewEnvelope(TypeTransferProgress, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got TransferProgress
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("TransferProgress = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_TransferProgress_WithError(t *testing.T) {
	want := TransferProgress{
		TransferID:   "xfer-1",
		Status:       "failed",
		ErrorMessage: "connection reset by peer",
	}

	env, err := NewEnvelope(TypeTransferProgress, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got TransferProgress
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("TransferProgress = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_StartEngineTransfer(t *testing.T) {
	want := StartEngineTransfer{TransferID: "engine-xfer-1", EngineType: "llamacpp", Version: "b4610"}

	env, err := NewEnvelope(TypeStartEngineTransfer, "req-5", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got StartEngineTransfer
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("StartEngineTransfer = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_EngineTransferProgress(t *testing.T) {
	want := EngineTransferProgress{
		TransferID:       "engine-xfer-1",
		BytesTransferred: 4096,
		BytesTotal:       8192,
		Status:           "transferring",
	}

	env, err := NewEnvelope(TypeEngineTransferProgress, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got EngineTransferProgress
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("EngineTransferProgress = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_EngineTransferProgress_Completed(t *testing.T) {
	want := EngineTransferProgress{
		TransferID:         "engine-xfer-1",
		Status:             "completed",
		InstallPath:        "/opt/sparky/serviceloop/engines/llamacpp/b4610",
		InstalledSizeBytes: 123456,
	}

	env, err := NewEnvelope(TypeEngineTransferProgress, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got EngineTransferProgress
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("EngineTransferProgress = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_EngineTransferProgress_Failed(t *testing.T) {
	want := EngineTransferProgress{
		TransferID:   "engine-xfer-1",
		Status:       "failed",
		ErrorMessage: "checksum mismatch",
	}

	env, err := NewEnvelope(TypeEngineTransferProgress, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got EngineTransferProgress
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("EngineTransferProgress = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_LoadInstance(t *testing.T) {
	want := LoadInstance{
		InstanceID:               "instance-1",
		ModelRef:                 "meta-llama/Llama-3-8B",
		Image:                    "vllm/vllm-openai:latest",
		Args:                     []string{"--tensor-parallel-size", "1"},
		Port:                     8000,
		RequiresFullGPUResidency: true,
	}

	env, err := NewEnvelope(TypeLoadInstance, "req-4", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got LoadInstance
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadInstance = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_UnloadInstance(t *testing.T) {
	want := UnloadInstance{InstanceID: "instance-1"}

	env, err := NewEnvelope(TypeUnloadInstance, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got UnloadInstance
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("UnloadInstance = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_InstanceResult(t *testing.T) {
	want := InstanceResult{InstanceID: "instance-1", Status: InstanceStatusRunning, ActualPort: 8000}

	env, err := NewEnvelope(TypeInstanceResult, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got InstanceResult
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("InstanceResult = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_InstanceResult_Failed(t *testing.T) {
	want := InstanceResult{InstanceID: "instance-1", Status: InstanceStatusFailed, ErrorMessage: "image pull failed"}

	env, err := NewEnvelope(TypeInstanceResult, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got InstanceResult
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("InstanceResult = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_Telemetry(t *testing.T) {
	want := Telemetry{
		RecordedAt:        time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC),
		GPUUtilizationPct: 45, GPUMemoryUsedMB: 8192, GPUMemoryTotalMB: 24576,
		CPUUtilizationPct: 12.5, SystemMemoryUsedMB: 4096, SystemMemoryTotalMB: 16384,
	}

	env, err := NewEnvelope(TypeTelemetry, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got Telemetry
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if !got.RecordedAt.Equal(want.RecordedAt) {
		t.Errorf("RecordedAt = %v, want %v", got.RecordedAt, want.RecordedAt)
	}
	got.RecordedAt, want.RecordedAt = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("Telemetry (excl. RecordedAt) = %+v, want %+v", got, want)
	}
}

func TestEnvelope_WireFormat(t *testing.T) {
	// Confirms the JSON field names actually on the wire match
	// ARCHITECTURE.md Protocol's snake_case convention (e.g. request_id),
	// not just that round-tripping through this package's own types works.
	env, err := NewEnvelope(TypeHello, "req-1", Hello{NodeName: "n", BearerToken: "t"})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	for _, field := range []string{"type", "request_id", "payload"} {
		if _, ok := asMap[field]; !ok {
			t.Errorf("wire JSON missing field %q: %s", field, raw)
		}
	}
}

func TestEnvelope_DecodePayload_MismatchedType(t *testing.T) {
	env, err := NewEnvelope(TypeHello, "req-1", Hello{NodeName: "n", BearerToken: "t"})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	// Hello's fields are both strings; HelloAck's Accepted field is a bool,
	// so decoding a Hello payload as a HelloAck must fail rather than
	// silently succeed with a zero-valued Accepted.
	var got HelloAck
	if err := env.DecodePayload(&got); err == nil {
		t.Fatal("DecodePayload() succeeded decoding a Hello payload as a HelloAck, want an error")
	}
}

func TestEnvelope_EmptyRequestID_OmittedFromWire(t *testing.T) {
	env, err := NewEnvelope(TypeHeartbeat, "", Heartbeat{SentAt: time.Now()})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}

	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	if _, ok := asMap["request_id"]; ok {
		t.Errorf("wire JSON has request_id set despite an empty value: %s", raw)
	}
}
