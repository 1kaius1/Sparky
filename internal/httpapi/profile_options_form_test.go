// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/launchopts"
)

func TestEnvFromText(t *testing.T) {
	raw, err := envFromText("# a comment\n\nVLLM_MARLIN_USE_ATOMIC_ADD=1\n  HF_HOME = /data/hf cache \r\nEMPTY=\nWITH_EQ=a=b\n")
	if err != nil {
		t.Fatalf("envFromText() error: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("result is not a JSON object: %v", err)
	}
	want := map[string]string{
		"VLLM_MARLIN_USE_ATOMIC_ADD": "1",
		"HF_HOME":                    "/data/hf cache",
		"EMPTY":                      "",
		"WITH_EQ":                    "a=b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}

	if raw, err := envFromText("  \n# only a comment\n"); err != nil || string(raw) != "{}" {
		t.Errorf("envFromText(blank) = %s, %v; want {}", raw, err)
	}
	for name, text := range map[string]string{
		"no equals":  "JUSTANAME",
		"duplicate":  "A=1\nA=2",
		"empty name": "=1",
	} {
		if name == "empty name" {
			// An empty name parses; launchopts rejects it when the profile is saved.
			if _, err := envFromText(text); err != nil {
				t.Errorf("%s: parse error %v, want the name check left to launchopts", name, err)
			}
			continue
		}
		if _, err := envFromText(text); err == nil {
			t.Errorf("%s: envFromText() accepted %q", name, text)
		}
	}
}

func TestEnvToText_SortedAndRoundTrips(t *testing.T) {
	text := envToText(json.RawMessage(`{"B":"2","A":"1"}`))
	if text != "A=1\nB=2" {
		t.Errorf("envToText() = %q", text)
	}
	raw, err := envFromText(text)
	if err != nil || string(raw) != `{"A":"1","B":"2"}` {
		t.Errorf("round trip = %s, %v", raw, err)
	}
	if envToText(json.RawMessage(`{}`)) != "" || envToText(json.RawMessage(`not json`)) != "" {
		t.Errorf("envToText should be empty for an empty or undecodable value")
	}
}

func TestParseMountLine(t *testing.T) {
	m, err := parseMountLine("/data/cache:/root/.cache:ro")
	if err != nil || m != (agentproto.ContainerMount{Source: "/data/cache", Target: "/root/.cache", ReadOnly: true}) {
		t.Errorf("parseMountLine(ro) = %+v, %v", m, err)
	}
	m, err = parseMountLine("/a:/b")
	if err != nil || m.ReadOnly || m.Source != "/a" || m.Target != "/b" {
		t.Errorf("parseMountLine(plain) = %+v, %v", m, err)
	}
	if m, err = parseMountLine("/a:/b:rw"); err != nil || m.ReadOnly {
		t.Errorf("parseMountLine(rw) = %+v, %v", m, err)
	}
	for _, bad := range []string{"/only", "/a:/b:ro:z", "/a:/b:wat", ""} {
		if _, err := parseMountLine(bad); err == nil {
			t.Errorf("parseMountLine(%q) accepted", bad)
		}
	}
}

func TestParseUlimitLine(t *testing.T) {
	u, err := parseUlimitLine("memlock=-1:-1")
	if err != nil || u != (agentproto.ContainerUlimit{Name: "memlock", Soft: -1, Hard: -1}) {
		t.Errorf("parseUlimitLine(unlimited) = %+v, %v", u, err)
	}
	u, err = parseUlimitLine("nofile=1024")
	if err != nil || u.Soft != 1024 || u.Hard != 1024 {
		t.Errorf("parseUlimitLine(single value) = %+v, %v; want soft and hard the same", u, err)
	}
	for _, bad := range []string{"nofile", "=1", "nofile=a:b", "nofile=1:"} {
		if _, err := parseUlimitLine(bad); err == nil {
			t.Errorf("parseUlimitLine(%q) accepted", bad)
		}
	}
}

func TestContainerOptionsFromForm_RoundTripsThroughApplyOptionsToForm(t *testing.T) {
	form := profileFormValues{
		ShmSizeGB: "16", IPCMode: "host", NetworkMode: "host",
		EntrypointText: "vllm\nserve", MemoryGB: "96.5", CPUs: "8", Init: true,
		AddHostsText: "db:10.0.0.5", TmpfsText: "/scratch",
		PIDMode: "host", Privileged: true,
		MountsText:  "/data:/data:ro\n/cache:/root/.cache",
		DevicesText: "/dev/infiniband/uverbs0", CapAddText: "IPC_LOCK, SYS_PTRACE", CapDropText: "NET_RAW",
		SecurityOptText: "seccomp=unconfined", UlimitsText: "memlock=-1:-1\nnofile=1024:4096",
	}
	raw, err := containerOptionsFromForm(form)
	if err != nil {
		t.Fatalf("containerOptionsFromForm() error: %v", err)
	}
	// Whatever the form produces must pass the validation the service applies.
	o, err := launchopts.ParseContainerOptions(raw)
	if err != nil {
		t.Fatalf("the form's options fail validation: %v\n%s", err, raw)
	}
	if o.ShmSizeGB == nil || *o.ShmSizeGB != 16 || len(o.Mounts) != 2 || !o.Mounts[0].ReadOnly || len(o.CapAdd) != 2 || len(o.Ulimits) != 2 {
		t.Errorf("unexpected options: %+v", o)
	}

	var back profileFormValues
	applyOptionsToForm(&back, json.RawMessage(`{}`), raw)
	// The cap lists come back one per line, not as typed.
	form.CapAddText = "IPC_LOCK\nSYS_PTRACE"
	if !reflect.DeepEqual(back, form) {
		t.Errorf("round trip changed the form:\n got %+v\nwant %+v", back, form)
	}
}

func TestContainerOptionsFromForm_BlankIsEmptyObject(t *testing.T) {
	raw, err := containerOptionsFromForm(profileFormValues{})
	if err != nil || string(raw) != "{}" {
		t.Errorf("containerOptionsFromForm(blank) = %s, %v; want {}", raw, err)
	}
}

func TestContainerOptionsFromForm_EntrypointClear(t *testing.T) {
	raw, err := containerOptionsFromForm(profileFormValues{EntrypointClear: true})
	if err != nil || string(raw) != `{"entrypoint":[""]}` {
		t.Errorf("containerOptionsFromForm(clear) = %s, %v", raw, err)
	}
	var back profileFormValues
	applyOptionsToForm(&back, nil, raw)
	if !back.EntrypointClear || back.EntrypointText != "" {
		t.Errorf("clear did not round trip: %+v", back)
	}
	if _, err := containerOptionsFromForm(profileFormValues{EntrypointClear: true, EntrypointText: "sh"}); err == nil {
		t.Error("clearing and replacing the entrypoint together was accepted")
	}
}

func TestContainerOptionsFromForm_ParseErrors(t *testing.T) {
	for name, f := range map[string]profileFormValues{
		"shm not a number":    {ShmSizeGB: "lots"},
		"memory not a number": {MemoryGB: "lots"},
		"cpus not a number":   {CPUs: "many"},
		"bad mount":           {MountsText: "/only"},
		"bad ulimit":          {UlimitsText: "nofile"},
	} {
		if _, err := containerOptionsFromForm(f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestApplyOptionsToForm_UndecodableValueLeavesFieldsBlank(t *testing.T) {
	var f profileFormValues
	applyOptionsToForm(&f, json.RawMessage(`{"A":"1"}`), json.RawMessage(`not json`))
	if f.EnvText != "A=1" || f.ShmSizeGB != "" || f.MountsText != "" {
		t.Errorf("form = %+v, want the env filled and the options blank", f)
	}
}

func profileFormAPI(t *testing.T, editor *fakeProfileEditor) *API {
	t.Helper()
	viewer := newFakeUserLister()
	viewer.byID["pd-1"] = &db.User{ID: "pd-1", Tier: db.TierPowerDev}
	viewer.byID["ad-1"] = &db.User{ID: "ad-1", Tier: db.TierAdmin}
	return newTestDashboardAPIWithProfileEditor(t, &fakeNodeLister{}, &fakeNodeRegistrar{}, &fakeProfileLister{}, editor, &fakeInstanceLister{}, &fakeTransferLister{}, viewer, &fakeAuditLister{}, &fakeUserRoster{}, &fakeUserElevator{}, &fakeSettingsViewer{}, &fakeMetricsLister{})
}

// Host-level inputs are shown to an Admin and not rendered at all for a
// PowerDev; the container-level ones, the environment and the general section
// are shown to both.
func TestHandleNewProfileForm_HostAccessInputsAreAdminOnly(t *testing.T) {
	hostInputs := []string{`name="privileged"`, `name="mounts"`, `name="devices"`, `name="cap_add"`, `name="ulimits"`, `name="pid_mode"`, `name="security_opt"`}
	shared := []string{`name="env"`, `name="shm_size_gb"`, `name="ipc_mode"`, `name="network_mode"`, `name="entrypoint"`, `name="memory_gb"`, `name="init"`, `name="image"`, `name="engine_params"`}

	for user, wantHost := range map[string]bool{"pd-1": false, "ad-1": true} {
		api := profileFormAPI(t, &fakeProfileEditor{})
		req := newAuthenticatedRequest(t, http.MethodGet, "/profiles/new", user)
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", user, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range shared {
			if !strings.Contains(body, want) {
				t.Errorf("%s: form is missing %s", user, want)
			}
		}
		for _, in := range hostInputs {
			if got := strings.Contains(body, in); got != wantHost {
				t.Errorf("%s: host-level input %s present = %v, want %v", user, in, got, wantHost)
			}
		}
	}
}

// The engine type dropdown offers what has an adapter; Aphrodite is deferred
// until after 1.0.0.
func TestHandleNewProfileForm_EngineTypesExcludeAphrodite(t *testing.T) {
	api := profileFormAPI(t, &fakeProfileEditor{})
	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles/new", "pd-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<option value="vllm"`) || !strings.Contains(body, `<option value="llamacpp"`) {
		t.Errorf("engine type dropdown is missing vllm or llamacpp: %s", body)
	}
	if strings.Contains(body, "aphrodite") {
		t.Errorf("engine type dropdown still offers aphrodite")
	}
}

func TestHandleCreateProfile_EnvAndContainerOptionsReachTheService(t *testing.T) {
	editor := &fakeProfileEditor{}
	api := profileFormAPI(t, editor)

	req := newAuthenticatedFormRequest(t, "/profiles/new", "ad-1", profileForm(url.Values{
		"env":          {"VLLM_MARLIN_USE_ATOMIC_ADD=1\nHF_HOME=/data/hf"},
		"shm_size_gb":  {"16"},
		"ipc_mode":     {"host"},
		"network_mode": {"host"},
		"entrypoint":   {"vllm\nserve"},
		"init":         {"yes"},
		"privileged":   {"yes"},
		"mounts":       {"/data:/data:ro"},
		"cap_add":      {"IPC_LOCK"},
		"ulimits":      {"memlock=-1:-1"},
	}))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if len(editor.created) != 1 {
		t.Fatalf("CreateProfile called %d times, want 1", len(editor.created))
	}
	p := editor.created[0]
	if string(p.Env) != `{"HF_HOME":"/data/hf","VLLM_MARLIN_USE_ATOMIC_ADD":"1"}` {
		t.Errorf("Env = %s", p.Env)
	}
	o, err := launchopts.ParseContainerOptions(p.ContainerOptions)
	if err != nil {
		t.Fatalf("ContainerOptions do not parse: %v\n%s", err, p.ContainerOptions)
	}
	if o.ShmSizeGB == nil || *o.ShmSizeGB != 16 || o.IPCMode != "host" || o.NetworkMode != "host" || !o.Init || !o.Privileged ||
		!reflect.DeepEqual(o.Entrypoint, []string{"vllm", "serve"}) || len(o.Mounts) != 1 || len(o.CapAdd) != 1 || len(o.Ulimits) != 1 {
		t.Errorf("ContainerOptions = %+v", o)
	}
}

// A line that is not NAME=value re-renders the form with everything the person
// typed still in it, and never reaches the service.
func TestHandleCreateProfile_BadEnvLineRerendersFormKeepingInput(t *testing.T) {
	editor := &fakeProfileEditor{}
	api := profileFormAPI(t, editor)

	req := newAuthenticatedFormRequest(t, "/profiles/new", "pd-1", profileForm(url.Values{
		"env":         {"GOOD=1\nnot an assignment"},
		"shm_size_gb": {"8"},
	}))
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "NAME=value") || !strings.Contains(body, "not an assignment") || !strings.Contains(body, `value="8"`) {
		t.Errorf("response does not show the error and keep the typed values: %s", body)
	}
	if len(editor.created) != 0 {
		t.Error("CreateProfile was called for an unparseable form")
	}
}

func TestHandleEditProfileForm_PrefillsEnvAndContainerOptions(t *testing.T) {
	nodeID := "node-1"
	editor := &fakeProfileEditor{getResult: &db.Profile{
		ID: "profile-1", Name: "llama-70b", ModelRef: "meta-llama/Llama-3-70B", EngineType: db.ProfileEngineVLLM,
		EngineParams: []byte(`{}`), Format: db.ModelFormatSafetensors, TargetNodeID: &nodeID, Port: 8001,
		Env:              []byte(`{"VLLM_MARLIN_USE_ATOMIC_ADD":"1"}`),
		ContainerOptions: []byte(`{"shm_size_gb":16,"ipc_mode":"host","mounts":[{"source":"/data","target":"/data","read_only":true}],"privileged":true}`),
	}}
	api := profileFormAPI(t, editor)

	// An Admin sees all of it.
	req := newAuthenticatedRequest(t, http.MethodGet, "/profiles/profile-1/edit", "ad-1")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"VLLM_MARLIN_USE_ATOMIC_ADD=1", `value="16"`, `<option value="host" selected>`, "/data:/data:ro", `name="privileged" value="yes" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("Admin edit form is missing %q", want)
		}
	}

	// A PowerDev sees the environment and the container-level options, but the
	// Admin's host-level ones are neither shown nor editable.
	req = newAuthenticatedRequest(t, http.MethodGet, "/profiles/profile-1/edit", "pd-1")
	rec = httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)
	body = rec.Body.String()
	if !strings.Contains(body, "VLLM_MARLIN_USE_ATOMIC_ADD=1") || !strings.Contains(body, `value="16"`) {
		t.Errorf("PowerDev edit form is missing the shared fields")
	}
	if strings.Contains(body, `name="mounts"`) || strings.Contains(body, "/data:/data:ro") || strings.Contains(body, `name="privileged"`) {
		t.Errorf("PowerDev edit form shows the Admin's host-level options")
	}
}
