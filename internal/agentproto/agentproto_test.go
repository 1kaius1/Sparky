// SPDX-License-Identifier: AGPL-3.0-or-later

package agentproto

import (
	"bytes"
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
	if got.EngineVersion != "" {
		t.Errorf("EngineVersion = %q, want empty (unpinned)", got.EngineVersion)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadInstance = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_LoadInstance_ShmSizeAndIPCMode(t *testing.T) {
	want := LoadInstance{
		InstanceID:               "instance-1",
		ModelRef:                 "meta-llama/Llama-3-8B",
		Image:                    "vllm/vllm-openai:latest",
		Port:                     8000,
		RequiresFullGPUResidency: true,
		ShmSize:                  16 * 1024 * 1024 * 1024,
		IPCMode:                  "host",
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

func TestEnvelope_RoundTrip_LoadInstance_PinnedEngineVersion(t *testing.T) {
	want := LoadInstance{
		InstanceID:    "instance-1",
		ModelRef:      "TinyLlama/TinyLlama-1.1B-Chat-v1.0-GGUF",
		EngineType:    "llamacpp",
		EngineVersion: "b4610",
		Port:          8001,
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

func TestEnvelope_RoundTrip_CheckInstance(t *testing.T) {
	want := CheckInstance{InstanceID: "instance-1"}

	env, err := NewEnvelope(TypeCheckInstance, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got CheckInstance
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("CheckInstance = %+v, want %+v", got, want)
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
		GPUs:              []GPUTelemetry{{Index: 0, UtilizationPct: 45, MemoryUsedMB: 8192, MemoryTotalMB: 24576}},
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
	if !reflect.DeepEqual(got, want) {
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

func TestEnvelope_RoundTrip_InstanceHealth_Healthy(t *testing.T) {
	checkedAt := time.Now().Truncate(time.Second).UTC()
	want := InstanceHealth{
		InstanceID: "instance-1",
		Status:     InstanceHealthStatusHealthy,
		CheckedAt:  checkedAt,
		Detail:     map[string]float64{"num_requests_running": 2, "num_requests_waiting": 0},
	}

	env, err := NewEnvelope(TypeInstanceHealth, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got InstanceHealth
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InstanceHealth = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_InstanceHealth_Unhealthy_NoDetail(t *testing.T) {
	checkedAt := time.Now().Truncate(time.Second).UTC()
	want := InstanceHealth{
		InstanceID: "instance-1",
		Status:     InstanceHealthStatusUnhealthy,
		CheckedAt:  checkedAt,
	}

	env, err := NewEnvelope(TypeInstanceHealth, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got InstanceHealth
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InstanceHealth = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_Hello_WithSSHIdentity(t *testing.T) {
	want := Hello{
		NodeName:         "spark-01",
		BearerToken:      "s3cr3t",
		SSHPublicKey:     "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... sparky-node",
		SSHHostPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... sshd-host",
	}

	env, err := NewEnvelope(TypeHello, "req-1", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got Hello
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("Hello = %+v, want %+v", got, want)
	}
}

func TestEnvelope_Hello_SSHFields_OmittedFromWire(t *testing.T) {
	// A not-yet-upgraded agent (or one whose sparky-agent setup step hasn't
	// produced a keypair yet) sends neither field - confirms omitempty
	// actually keeps them off the wire rather than sending empty strings.
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
	payload, ok := asMap["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload is not a JSON object: %v", asMap["payload"])
	}
	for _, field := range []string{"ssh_public_key", "ssh_host_public_key"} {
		if _, ok := payload[field]; ok {
			t.Errorf("wire JSON has %q set despite an empty value: %s", field, raw)
		}
	}
}

func TestEnvelope_RoundTrip_ReportInterfaces(t *testing.T) {
	speed := 10000
	want := ReportInterfaces{
		Interfaces: []NetworkInterface{
			{Name: "eth0", IPAddress: "10.0.0.5", LinkSpeedMbps: &speed},
			{Name: "eth1", IPAddress: "10.0.1.5"},
		},
	}

	env, err := NewEnvelope(TypeReportInterfaces, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got ReportInterfaces
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReportInterfaces = %+v, want %+v", got, want)
	}
	if got.Interfaces[1].LinkSpeedMbps != nil {
		t.Errorf("Interfaces[1].LinkSpeedMbps = %v, want nil (unknown link speed)", *got.Interfaces[1].LinkSpeedMbps)
	}
}

func TestEnvelope_NetworkInterface_LinkSpeedMbps_OmittedWhenNil(t *testing.T) {
	env, err := NewEnvelope(TypeReportInterfaces, "", ReportInterfaces{
		Interfaces: []NetworkInterface{{Name: "eth0", IPAddress: "10.0.0.5"}},
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if bytes.Contains(raw, []byte("link_speed_mbps")) {
		t.Errorf("wire JSON has link_speed_mbps set despite a nil value: %s", raw)
	}
}

func TestEnvelope_RoundTrip_RescanInterfaces(t *testing.T) {
	env, err := NewEnvelope(TypeRescanInterfaces, "req-6", RescanInterfaces{})
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got RescanInterfaces
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != (RescanInterfaces{}) {
		t.Errorf("RescanInterfaces = %+v, want zero value", got)
	}
}

func TestEnvelope_RoundTrip_AuthorizePeerPull(t *testing.T) {
	want := AuthorizePeerPull{
		TransferID:    "xfer-1",
		DestPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... dest-node",
		DestIPAddress: "10.0.0.7",
		ModelRef:      "meta-llama/Llama-3-8B",
		Quantization:  "Q4_K_M",
		Format:        "gguf",
	}

	env, err := NewEnvelope(TypeAuthorizePeerPull, "req-7", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got AuthorizePeerPull
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("AuthorizePeerPull = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_PeerAuthorizeResult(t *testing.T) {
	want := PeerAuthorizeResult{TransferID: "xfer-1", Accepted: true}

	env, err := NewEnvelope(TypePeerAuthorizeResult, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got PeerAuthorizeResult
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("PeerAuthorizeResult = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_PeerAuthorizeResult_Rejected(t *testing.T) {
	want := PeerAuthorizeResult{TransferID: "xfer-1", Accepted: false, Reason: "model not present locally"}

	env, err := NewEnvelope(TypePeerAuthorizeResult, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got PeerAuthorizeResult
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("PeerAuthorizeResult = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_StartPeerTransfer(t *testing.T) {
	want := StartPeerTransfer{
		TransferID:          "xfer-1",
		SourceNodeID:        "node-1",
		SourceHost:          "10.0.0.5",
		SourceSSHPort:       22,
		SourceHostPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... source-sshd",
		ModelRef:            "meta-llama/Llama-3-8B",
		Quantization:        "Q4_K_M",
		Format:              "gguf",
	}

	env, err := NewEnvelope(TypeStartPeerTransfer, "req-8", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got StartPeerTransfer
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("StartPeerTransfer = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_RevokePeerPull(t *testing.T) {
	want := RevokePeerPull{TransferID: "xfer-1"}

	env, err := NewEnvelope(TypeRevokePeerPull, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got RevokePeerPull
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("RevokePeerPull = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_CheckPeerConnectivity(t *testing.T) {
	want := CheckPeerConnectivity{CheckID: "check-1", SourceHost: "10.0.0.5", SourceSSHPort: 22}

	env, err := NewEnvelope(TypeCheckPeerConnectivity, "req-9", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got CheckPeerConnectivity
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("CheckPeerConnectivity = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_ConnectivityCheckResult(t *testing.T) {
	want := ConnectivityCheckResult{CheckID: "check-1", Reachable: true, LatencyMs: 4}

	env, err := NewEnvelope(TypeConnectivityCheckResult, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got ConnectivityCheckResult
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("ConnectivityCheckResult = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_ConnectivityCheckResult_Unreachable(t *testing.T) {
	want := ConnectivityCheckResult{CheckID: "check-1", Reachable: false, Reason: "dial tcp: connection refused"}

	env, err := NewEnvelope(TypeConnectivityCheckResult, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got ConnectivityCheckResult
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("ConnectivityCheckResult = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_CancelTransfer(t *testing.T) {
	want := CancelTransfer{TransferID: "xfer-1"}

	env, err := NewEnvelope(TypeCancelTransfer, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got CancelTransfer
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("CancelTransfer = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_DeleteModel(t *testing.T) {
	want := DeleteModel{ModelRef: "meta-llama/Llama-3-8B", Quantization: "Q4_K_M", Format: "gguf"}

	env, err := NewEnvelope(TypeDeleteModel, "req-10", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got DeleteModel
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("DeleteModel = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_DeleteModelResult(t *testing.T) {
	want := DeleteModelResult{ModelRef: "meta-llama/Llama-3-8B", Quantization: "Q4_K_M", Format: "gguf", Success: true}

	env, err := NewEnvelope(TypeDeleteModelResult, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got DeleteModelResult
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("DeleteModelResult = %+v, want %+v", got, want)
	}
}

func TestEnvelope_RoundTrip_DeleteModelResult_Failed(t *testing.T) {
	want := DeleteModelResult{ModelRef: "meta-llama/Llama-3-8B", Success: false, Reason: "path does not exist"}

	env, err := NewEnvelope(TypeDeleteModelResult, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope() error: %v", err)
	}

	var got DeleteModelResult
	if err := env.DecodePayload(&got); err != nil {
		t.Fatalf("DecodePayload() error: %v", err)
	}
	if got != want {
		t.Errorf("DeleteModelResult = %+v, want %+v", got, want)
	}
}
