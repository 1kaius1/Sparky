// SPDX-License-Identifier: AGPL-3.0-or-later

package launchopts

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

func TestParseEnv(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", "  "} {
		env, err := ParseEnv(json.RawMessage(raw))
		if err != nil || len(env) != 0 {
			t.Errorf("ParseEnv(%q) = %v, %v; want empty, nil", raw, env, err)
		}
	}

	env, err := ParseEnv(json.RawMessage(`{"VLLM_MARLIN_USE_ATOMIC_ADD":"1","_X":"a=b"}`))
	if err != nil {
		t.Fatalf("ParseEnv valid: %v", err)
	}
	if env["VLLM_MARLIN_USE_ATOMIC_ADD"] != "1" || env["_X"] != "a=b" {
		t.Errorf("ParseEnv = %v", env)
	}

	bad := map[string]string{
		"name with equals": `{"A=B":"1"}`,
		"name with space":  `{"A B":"1"}`,
		"name with digit":  `{"1A":"1"}`,
		"empty name":       `{"":"1"}`,
		"non-string value": `{"A":1}`,
		"not an object":    `["A=1"]`,
		"nul in value":     `{"A":"x\u0000y"}`,
		"value too long":   `{"A":"` + strings.Repeat("x", maxEnvValueLen+1) + `"}`,
	}
	for name, raw := range bad {
		if _, err := ParseEnv(json.RawMessage(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestParseEnv_TooManyVariables(t *testing.T) {
	m := map[string]string{}
	for i := 0; i <= maxEnvVars; i++ {
		m["V"+strings.Repeat("a", i%5)+string(rune('A'+i%26))+strings.Repeat("b", i/26)] = "1"
	}
	raw, _ := json.Marshal(m)
	if len(m) <= maxEnvVars {
		t.Fatalf("test setup: only %d distinct names", len(m))
	}
	if _, err := ParseEnv(raw); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestEnvList_SortedAndStable(t *testing.T) {
	got := EnvList(map[string]string{"B": "2", "A": "1", "C": "x=y"})
	want := []string{"A=1", "B=2", "C=x=y"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("EnvList = %v, want %v", got, want)
	}
	if EnvList(nil) != nil {
		t.Errorf("EnvList(nil) should be nil")
	}
}

func TestParseContainerOptions_Valid(t *testing.T) {
	raw := `{
		"shm_size_gb": 16, "ipc_mode": "host", "network_mode": "host",
		"entrypoint": ["vllm","serve"], "memory_gb": 96.5, "cpus": 8, "init": true,
		"add_hosts": ["db:10.0.0.5","gw:host-gateway"], "tmpfs": ["/tmp/x"],
		"pid_mode": "host", "privileged": true,
		"mounts": [{"source":"/data/cache","target":"/root/.cache","read_only":true}],
		"devices": ["/dev/infiniband/uverbs0"], "cap_add": ["IPC_LOCK","CAP_SYS_PTRACE"], "cap_drop": ["ALL"],
		"security_opt": ["seccomp=unconfined"],
		"ulimits": [{"name":"memlock","soft":-1,"hard":-1},{"name":"nofile","soft":1024,"hard":65536}]
	}`
	o, err := ParseContainerOptions(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("ParseContainerOptions: %v", err)
	}
	if o.ShmSizeGB == nil || *o.ShmSizeGB != 16 || o.IPCMode != "host" || !o.Privileged || len(o.Ulimits) != 2 {
		t.Errorf("unexpected decode: %+v", o)
	}
}

func TestParseContainerOptions_Empty(t *testing.T) {
	for _, raw := range []string{"", "null", "{}"} {
		o, err := ParseContainerOptions(json.RawMessage(raw))
		if err != nil || !IsZero(o) {
			t.Errorf("ParseContainerOptions(%q) = %+v, %v; want zero, nil", raw, o, err)
		}
	}
}

func TestParseContainerOptions_Rejected(t *testing.T) {
	cases := map[string]string{
		"unknown key":          `{"shm":4}`,
		"not an object":        `[1]`,
		"shm zero":             `{"shm_size_gb":0}`,
		"shm huge":             `{"shm_size_gb":99999}`,
		"bad ipc":              `{"ipc_mode":"container:abc"}`,
		"bad network":          `{"network_mode":"container:abc"}`,
		"bad pid":              `{"pid_mode":"private"}`,
		"empty entrypoint arg": `{"entrypoint":["vllm",""]}`,
		"memory zero":          `{"memory_gb":0}`,
		"cpus negative":        `{"cpus":-1}`,
		"add_host no addr":     `{"add_hosts":["db"]}`,
		"add_host bad ip":      `{"add_hosts":["db:nope"]}`,
		"tmpfs relative":       `{"tmpfs":["tmp"]}`,
		"mount relative":       `{"mounts":[{"source":"data","target":"/x"}]}`,
		"mount colon":          `{"mounts":[{"source":"/a:/etc","target":"/x"}]}`,
		"mount comma":          `{"mounts":[{"source":"/a","target":"/x,rw"}]}`,
		"device outside dev":   `{"devices":["/etc/passwd"]}`,
		"device traversal":     `{"devices":["/dev/../etc/passwd"]}`,
		"cap lowercase":        `{"cap_add":["sys_ptrace"]}`,
		"security_opt blank":   `{"security_opt":[" "]}`,
		"ulimit unknown":       `{"ulimits":[{"name":"bogus","soft":1,"hard":1}]}`,
		"ulimit dup":           `{"ulimits":[{"name":"nofile","soft":1,"hard":2},{"name":"nofile","soft":1,"hard":2}]}`,
		"ulimit soft>hard":     `{"ulimits":[{"name":"nofile","soft":5,"hard":2}]}`,
		"ulimit below -1":      `{"ulimits":[{"name":"nofile","soft":-2,"hard":2}]}`,
	}
	for name, raw := range cases {
		if _, err := ParseContainerOptions(json.RawMessage(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestParseContainerOptions_EntrypointClearAllowed(t *testing.T) {
	o, err := ParseContainerOptions(json.RawMessage(`{"entrypoint":[""]}`))
	if err != nil {
		t.Fatalf("a single empty entrypoint clears the image's: %v", err)
	}
	if len(o.Entrypoint) != 1 || o.Entrypoint[0] != "" {
		t.Errorf("Entrypoint = %q", o.Entrypoint)
	}
}

func TestHostLevelSplit(t *testing.T) {
	shm := 8
	full := agentproto.ContainerOptions{
		ShmSizeGB: &shm, NetworkMode: "host",
		Privileged: true, PIDMode: "host",
		Mounts:  []agentproto.ContainerMount{{Source: "/a", Target: "/b"}},
		CapAdd:  []string{"IPC_LOCK"},
		Ulimits: []agentproto.ContainerUlimit{{Name: "memlock", Soft: -1, Hard: -1}},
	}

	host := HostLevel(full)
	if host.ShmSizeGB != nil || host.NetworkMode != "" {
		t.Errorf("HostLevel kept a non-host field: %+v", host)
	}
	if !host.Privileged || len(host.Mounts) != 1 || len(host.CapAdd) != 1 || len(host.Ulimits) != 1 || host.PIDMode != "host" {
		t.Errorf("HostLevel lost a host field: %+v", host)
	}

	// A PowerDev edit: changes the shm size and drops the host-level group
	// (the form never carried it); WithHostLevel puts the Admin's back.
	newShm := 32
	edit := agentproto.ContainerOptions{ShmSizeGB: &newShm}
	merged := WithHostLevel(edit, full)
	if merged.ShmSizeGB == nil || *merged.ShmSizeGB != 32 {
		t.Errorf("WithHostLevel changed a non-host field: %+v", merged)
	}
	if !HostLevelEqual(merged, full) {
		t.Errorf("host-level fields were not carried over: %+v", merged)
	}
	if HostLevelEqual(edit, full) {
		t.Errorf("HostLevelEqual should see that edit dropped the Admin's fields")
	}
}

func TestHostLevelEqual_EmptyListEqualsAbsent(t *testing.T) {
	a := agentproto.ContainerOptions{Mounts: []agentproto.ContainerMount{}}
	if !HostLevelEqual(a, agentproto.ContainerOptions{}) {
		t.Errorf("an empty list and an absent one should compare equal")
	}
}

func TestMarshal_EmptyIsObject(t *testing.T) {
	b, err := Marshal(agentproto.ContainerOptions{})
	if err != nil || string(b) != "{}" {
		t.Errorf("Marshal(zero) = %s, %v; want {}", b, err)
	}
}
