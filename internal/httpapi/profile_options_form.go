// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/1kaius1/Sparky/internal/agentproto"
	"github.com/1kaius1/Sparky/internal/launchopts"
)

// The profile form edits environment variables and container options as
// plain text (one entry per line, in the notation of `docker run` and
// `.env` files) rather than as JSON, because that is how people already write
// them. This file converts between that text and the JSON the service takes
// and the database stores; what the values may be is internal/launchopts's
// job, so nothing here validates beyond what is needed to parse a line.

// readProfileForm collects every profile form field from a parsed request.
// Fields the viewer's form did not render (the host-level container options for
// someone who may not set them) simply arrive empty.
func readProfileForm(r *http.Request) profileFormValues {
	return profileFormValues{
		Name:             r.PostFormValue("name"),
		EngineType:       r.PostFormValue("engine_type"),
		EngineParamsJSON: r.PostFormValue("engine_params"),
		RequiredMemoryGB: r.PostFormValue("required_memory_gb"),
		EngineVersion:    r.PostFormValue("engine_version"),
		Image:            r.PostFormValue("image"),
		TargetNodeID:     r.PostFormValue("target_node_id"),
		Entry:            r.PostFormValue("entry"),
		Port:             r.PostFormValue("port"),

		EnvText: r.PostFormValue("env"),

		ShmSizeGB:       r.PostFormValue("shm_size_gb"),
		IPCMode:         r.PostFormValue("ipc_mode"),
		NetworkMode:     r.PostFormValue("network_mode"),
		EntrypointText:  r.PostFormValue("entrypoint"),
		EntrypointClear: r.PostFormValue("entrypoint_clear") != "",
		MemoryGB:        r.PostFormValue("memory_gb"),
		CPUs:            r.PostFormValue("cpus"),
		Init:            r.PostFormValue("init") != "",
		AddHostsText:    r.PostFormValue("add_hosts"),
		TmpfsText:       r.PostFormValue("tmpfs"),

		PIDMode:         r.PostFormValue("pid_mode"),
		Privileged:      r.PostFormValue("privileged") != "",
		MountsText:      r.PostFormValue("mounts"),
		DevicesText:     r.PostFormValue("devices"),
		CapAddText:      r.PostFormValue("cap_add"),
		CapDropText:     r.PostFormValue("cap_drop"),
		SecurityOptText: r.PostFormValue("security_opt"),
		UlimitsText:     r.PostFormValue("ulimits"),
	}
}

// textLines splits a textarea into its non-blank lines, trimmed.
func textLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// textWords splits on commas and any whitespace, for lists of short names
// (capabilities) that people write either way.
func textWords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

// envFromText parses `.env`-style text: NAME=value per line, blank lines and
// lines starting with '#' ignored. The value is everything after the first
// '=' with surrounding whitespace trimmed, and is taken literally - no quote
// handling and no ${expansion}, since the text is not interpreted by a shell.
func envFromText(text string) (json.RawMessage, error) {
	env := map[string]string{}
	for _, line := range textLines(text) {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("environment variable line %q must be NAME=value", line)
		}
		name = strings.TrimSpace(name)
		if _, dup := env[name]; dup {
			return nil, fmt.Errorf("environment variable %s is set twice", name)
		}
		env[name] = strings.TrimSpace(value)
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("encode environment variables: %w", err)
	}
	return b, nil
}

// envToText is the inverse of envFromText, sorted by name.
func envToText(raw json.RawMessage) string {
	var env map[string]string
	if err := json.Unmarshal(raw, &env); err != nil || len(env) == 0 {
		return ""
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, name+"="+env[name])
	}
	return strings.Join(lines, "\n")
}

// parseMountLine reads "source:target" or "source:target:ro|rw", the notation
// of `docker run -v`. Paths containing ':' cannot be written this way, and
// internal/launchopts refuses them anyway.
func parseMountLine(line string) (agentproto.ContainerMount, error) {
	parts := strings.Split(line, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return agentproto.ContainerMount{}, fmt.Errorf("mount %q must be source:target or source:target:ro", line)
	}
	m := agentproto.ContainerMount{Source: parts[0], Target: parts[1]}
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			m.ReadOnly = true
		case "rw":
		default:
			return agentproto.ContainerMount{}, fmt.Errorf("mount %q: the last part must be ro or rw", line)
		}
	}
	return m, nil
}

// parseUlimitLine reads "name=soft:hard" or "name=value" (soft and hard the
// same), the notation of `docker run --ulimit`. -1 means unlimited.
func parseUlimitLine(line string) (agentproto.ContainerUlimit, error) {
	name, limits, ok := strings.Cut(line, "=")
	if !ok || name == "" {
		return agentproto.ContainerUlimit{}, fmt.Errorf("ulimit %q must be name=soft:hard or name=value", line)
	}
	softText, hardText, both := strings.Cut(limits, ":")
	if !both {
		hardText = softText
	}
	soft, err1 := strconv.ParseInt(softText, 10, 64)
	hard, err2 := strconv.ParseInt(hardText, 10, 64)
	if err1 != nil || err2 != nil {
		return agentproto.ContainerUlimit{}, fmt.Errorf("ulimit %q: the limits must be whole numbers (-1 for unlimited)", line)
	}
	return agentproto.ContainerUlimit{Name: name, Soft: soft, Hard: hard}, nil
}

func ulimitLine(u agentproto.ContainerUlimit) string {
	return fmt.Sprintf("%s=%d:%d", u.Name, u.Soft, u.Hard)
}

func mountLine(m agentproto.ContainerMount) string {
	s := m.Source + ":" + m.Target
	if m.ReadOnly {
		s += ":ro"
	}
	return s
}

// containerOptionsFromForm builds the container_options JSON from the form's
// text fields. Only parse errors are reported here; whether the values are
// acceptable is launchopts's call, made when the profile is saved.
func containerOptionsFromForm(f profileFormValues) (json.RawMessage, error) {
	o := agentproto.ContainerOptions{
		IPCMode:     f.IPCMode,
		NetworkMode: f.NetworkMode,
		PIDMode:     f.PIDMode,
		Init:        f.Init,
		Privileged:  f.Privileged,
		AddHosts:    textLines(f.AddHostsText),
		Tmpfs:       textLines(f.TmpfsText),
		Devices:     textLines(f.DevicesText),
		CapAdd:      textWords(f.CapAddText),
		CapDrop:     textWords(f.CapDropText),
		SecurityOpt: textLines(f.SecurityOptText),
	}

	if v := strings.TrimSpace(f.ShmSizeGB); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("shared memory size must be a whole number of GiB")
		}
		o.ShmSizeGB = &n
	}
	if v := strings.TrimSpace(f.MemoryGB); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("memory limit must be a number of GiB")
		}
		o.MemoryGB = &n
	}
	if v := strings.TrimSpace(f.CPUs); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("CPU limit must be a number")
		}
		o.CPUs = &n
	}

	entry := textLines(f.EntrypointText)
	switch {
	case f.EntrypointClear && len(entry) > 0:
		return nil, fmt.Errorf("either clear the image's entrypoint or give a replacement, not both")
	case f.EntrypointClear:
		// A single empty entry is how a container API clears the entrypoint.
		o.Entrypoint = []string{""}
	default:
		o.Entrypoint = entry
	}

	for _, line := range textLines(f.MountsText) {
		m, err := parseMountLine(line)
		if err != nil {
			return nil, err
		}
		o.Mounts = append(o.Mounts, m)
	}
	for _, line := range textLines(f.UlimitsText) {
		u, err := parseUlimitLine(line)
		if err != nil {
			return nil, err
		}
		o.Ulimits = append(o.Ulimits, u)
	}
	return launchopts.Marshal(o)
}

// applyOptionsToForm fills the environment and container option fields of a
// form from a stored profile - the inverse of envFromText and
// containerOptionsFromForm. A stored value that does not decode leaves its
// fields blank rather than blocking the edit page; saving then replaces it.
func applyOptionsToForm(f *profileFormValues, env, containerOptions json.RawMessage) {
	f.EnvText = envToText(env)

	var o agentproto.ContainerOptions
	if err := json.Unmarshal(containerOptions, &o); err != nil {
		return
	}
	if o.ShmSizeGB != nil {
		f.ShmSizeGB = strconv.Itoa(*o.ShmSizeGB)
	}
	f.IPCMode = o.IPCMode
	f.NetworkMode = o.NetworkMode
	f.PIDMode = o.PIDMode
	if len(o.Entrypoint) == 1 && o.Entrypoint[0] == "" {
		f.EntrypointClear = true
	} else {
		f.EntrypointText = strings.Join(o.Entrypoint, "\n")
	}
	if o.MemoryGB != nil {
		f.MemoryGB = strconv.FormatFloat(*o.MemoryGB, 'f', -1, 64)
	}
	if o.CPUs != nil {
		f.CPUs = strconv.FormatFloat(*o.CPUs, 'f', -1, 64)
	}
	f.Init = o.Init
	f.Privileged = o.Privileged
	f.AddHostsText = strings.Join(o.AddHosts, "\n")
	f.TmpfsText = strings.Join(o.Tmpfs, "\n")
	f.DevicesText = strings.Join(o.Devices, "\n")
	f.CapAddText = strings.Join(o.CapAdd, "\n")
	f.CapDropText = strings.Join(o.CapDrop, "\n")
	f.SecurityOptText = strings.Join(o.SecurityOpt, "\n")
	for _, m := range o.Mounts {
		f.MountsText += mountLine(m) + "\n"
	}
	f.MountsText = strings.TrimSuffix(f.MountsText, "\n")
	for _, u := range o.Ulimits {
		f.UlimitsText += ulimitLine(u) + "\n"
	}
	f.UlimitsText = strings.TrimSuffix(f.UlimitsText, "\n")
}
