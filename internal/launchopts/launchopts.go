// SPDX-License-Identifier: AGPL-3.0-or-later

// Package launchopts validates the parts of a model profile that describe the
// environment an engine runs in rather than the engine's own flags: its
// environment variables and its container-runtime options (SCHEMA.md Model
// profiles' env and container_options). Engine flags are validated by
// internal/engines. Like that package, this one never touches the database or
// a node; internal/profiles calls it when a profile is saved and
// internal/lifecycle when one is launched.
//
// The container options are agentproto.ContainerOptions itself - the stored
// JSON and the wire payload are the same shape - so this package only adds the
// rules: what values are acceptable, and which of the fields reach into the
// host and so are an Admin's to set (HostLevel).
package launchopts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

// ErrInvalid is returned, wrapped with a specific reason, when env or
// container options fail validation - the same pattern as
// internal/engines' ErrInvalidParams, so callers can show the message
// directly.
var ErrInvalid = errors.New("invalid launch options")

const (
	maxEnvVars     = 128
	maxEnvValueLen = 8192
	maxListEntries = 64
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// ParseEnv decodes a profile's env column: a JSON object of string values.
// An empty column, null, or {} is an empty environment. The names are checked
// because they end up as KEY=VALUE entries - a name containing "=" would
// silently turn into a different variable.
func ParseEnv(raw json.RawMessage) (map[string]string, error) {
	env := map[string]string{}
	if isEmptyJSON(raw) {
		return env, nil
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%w: env must be an object of text values: %v", ErrInvalid, err)
	}
	if err := ValidateEnv(env); err != nil {
		return nil, err
	}
	return env, nil
}

// ValidateEnv checks an environment map's names and values. The agent runs it
// again on what the central app sends, so a node never starts an engine from
// an entry that could redefine another variable.
func ValidateEnv(env map[string]string) error {
	if len(env) > maxEnvVars {
		return fmt.Errorf("%w: env has %d variables, the limit is %d", ErrInvalid, len(env), maxEnvVars)
	}
	for name, value := range env {
		if !envNameRE.MatchString(name) {
			return fmt.Errorf("%w: environment variable name %q must start with a letter or underscore and contain only letters, digits and underscores", ErrInvalid, name)
		}
		if len(value) > maxEnvValueLen {
			return fmt.Errorf("%w: environment variable %s is longer than %d characters", ErrInvalid, name, maxEnvValueLen)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("%w: environment variable %s contains a NUL character", ErrInvalid, name)
		}
	}
	return nil
}

// EnvList turns an environment map into KEY=VALUE entries in a stable order,
// the form both container runtimes and exec take. Sorted so two launches of
// one profile produce an identical container configuration.
func EnvList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+env[name])
	}
	return out
}

// ParseContainerOptions decodes and validates a profile's container_options
// column. An unknown key is an error rather than being ignored, for the same
// reason as engine_params: a key that is dropped silently gives the operator
// no sign that what they set never reached the container.
func ParseContainerOptions(raw json.RawMessage) (agentproto.ContainerOptions, error) {
	var o agentproto.ContainerOptions
	if isEmptyJSON(raw) {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return agentproto.ContainerOptions{}, fmt.Errorf("%w: container options: %v", ErrInvalid, err)
	}
	if err := Validate(o); err != nil {
		return agentproto.ContainerOptions{}, err
	}
	return o, nil
}

// Marshal encodes options for the container_options column. No options at all
// is {} (the column is NOT NULL), never null.
func Marshal(o agentproto.ContainerOptions) (json.RawMessage, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("encode container options: %w", err)
	}
	return b, nil
}

// Validate checks o's values. It does not check who may set them (HostLevel)
// or whether the node's runtime backend has containers at all - the caller
// that knows the node does that.
func Validate(o agentproto.ContainerOptions) error {
	if o.ShmSizeGB != nil && (*o.ShmSizeGB < 1 || *o.ShmSizeGB > 4096) {
		return fmt.Errorf("%w: shm_size_gb must be between 1 and 4096, got %d", ErrInvalid, *o.ShmSizeGB)
	}
	switch o.IPCMode {
	case "", "private", "shareable", "host":
	default:
		return fmt.Errorf("%w: ipc_mode must be private, shareable or host, got %q", ErrInvalid, o.IPCMode)
	}
	switch o.NetworkMode {
	case "", "bridge", "host":
	default:
		return fmt.Errorf("%w: network_mode must be bridge or host, got %q", ErrInvalid, o.NetworkMode)
	}
	switch o.PIDMode {
	case "", "host":
	default:
		return fmt.Errorf("%w: pid_mode must be host, got %q", ErrInvalid, o.PIDMode)
	}
	if err := validateEntrypoint(o.Entrypoint); err != nil {
		return err
	}
	if o.MemoryGB != nil && *o.MemoryGB <= 0 {
		return fmt.Errorf("%w: memory_gb must be positive, got %v", ErrInvalid, *o.MemoryGB)
	}
	if o.CPUs != nil && *o.CPUs <= 0 {
		return fmt.Errorf("%w: cpus must be positive, got %v", ErrInvalid, *o.CPUs)
	}
	for name, n := range map[string]int{
		"add_hosts": len(o.AddHosts), "tmpfs": len(o.Tmpfs), "mounts": len(o.Mounts),
		"devices": len(o.Devices), "cap_add": len(o.CapAdd), "cap_drop": len(o.CapDrop),
		"security_opt": len(o.SecurityOpt), "ulimits": len(o.Ulimits), "entrypoint": len(o.Entrypoint),
	} {
		if n > maxListEntries {
			return fmt.Errorf("%w: %s has %d entries, the limit is %d", ErrInvalid, name, n, maxListEntries)
		}
	}
	for _, h := range o.AddHosts {
		if err := validateAddHost(h); err != nil {
			return err
		}
	}
	for _, p := range o.Tmpfs {
		if err := validateAbsPath("tmpfs path", p); err != nil {
			return err
		}
	}
	for _, m := range o.Mounts {
		if err := validateMount(m); err != nil {
			return err
		}
	}
	for _, d := range o.Devices {
		if err := validateDevice(d); err != nil {
			return err
		}
	}
	for _, c := range o.CapAdd {
		if err := validateCap("cap_add", c); err != nil {
			return err
		}
	}
	for _, c := range o.CapDrop {
		if err := validateCap("cap_drop", c); err != nil {
			return err
		}
	}
	for _, s := range o.SecurityOpt {
		if strings.TrimSpace(s) == "" || strings.ContainsRune(s, 0) {
			return fmt.Errorf("%w: security_opt entries must be non-empty text", ErrInvalid)
		}
	}
	return validateUlimits(o.Ulimits)
}

func validateEntrypoint(e []string) error {
	// A single empty string is docker's way to clear the image's entrypoint.
	if len(e) == 1 && e[0] == "" {
		return nil
	}
	for _, part := range e {
		if part == "" || strings.ContainsRune(part, 0) {
			return fmt.Errorf("%w: entrypoint entries must be non-empty (use a single empty entry to clear the image's entrypoint)", ErrInvalid)
		}
	}
	return nil
}

func validateAddHost(h string) error {
	name, addr, ok := strings.Cut(h, ":")
	if !ok || name == "" || addr == "" || strings.ContainsAny(name, " \t,") {
		return fmt.Errorf("%w: add_hosts entry %q must be name:address", ErrInvalid, h)
	}
	// docker's own special value, resolved to the host's gateway address.
	if addr != "host-gateway" && net.ParseIP(addr) == nil {
		return fmt.Errorf("%w: add_hosts entry %q: %q is not an IP address", ErrInvalid, h, addr)
	}
	return nil
}

func validateAbsPath(what, p string) error {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, ":,\x00") {
		return fmt.Errorf("%w: %s %q must be an absolute path without ':' or ','", ErrInvalid, what, p)
	}
	return nil
}

// validateMount rejects ':' and ',' in either path because the agent builds a
// "source:target:mode" string from them (and Podman splits options on a comma);
// a path containing either would change what is mounted where.
func validateMount(m agentproto.ContainerMount) error {
	if err := validateAbsPath("mount source", m.Source); err != nil {
		return err
	}
	return validateAbsPath("mount target", m.Target)
}

func validateDevice(d string) error {
	if !strings.HasPrefix(d, "/dev/") || strings.ContainsAny(d, ":,\x00 ") || strings.Contains(d, "..") {
		return fmt.Errorf("%w: device %q must be a path under /dev", ErrInvalid, d)
	}
	return nil
}

var capRE = regexp.MustCompile(`^(CAP_)?[A-Z][A-Z0-9_]*$`)

func validateCap(field, c string) error {
	if !capRE.MatchString(c) {
		return fmt.Errorf("%w: %s entry %q must be a capability name such as SYS_PTRACE, or ALL", ErrInvalid, field, c)
	}
	return nil
}

// ulimitNames are the names docker's --ulimit accepts.
var ulimitNames = map[string]bool{
	"core": true, "cpu": true, "data": true, "fsize": true, "locks": true,
	"memlock": true, "msgqueue": true, "nice": true, "nofile": true,
	"nproc": true, "rss": true, "rtprio": true, "rttime": true,
	"sigpending": true, "stack": true,
}

func validateUlimits(us []agentproto.ContainerUlimit) error {
	seen := map[string]bool{}
	for _, u := range us {
		if !ulimitNames[u.Name] {
			return fmt.Errorf("%w: ulimit %q is not a known limit name (memlock, nofile, stack, ...)", ErrInvalid, u.Name)
		}
		if seen[u.Name] {
			return fmt.Errorf("%w: ulimit %q is set twice", ErrInvalid, u.Name)
		}
		seen[u.Name] = true
		if u.Soft < -1 || u.Hard < -1 {
			return fmt.Errorf("%w: ulimit %s: soft and hard must be -1 (unlimited) or positive", ErrInvalid, u.Name)
		}
		if u.Soft >= 0 && u.Hard >= 0 && u.Soft > u.Hard {
			return fmt.Errorf("%w: ulimit %s: soft (%d) is above hard (%d)", ErrInvalid, u.Name, u.Soft, u.Hard)
		}
	}
	return nil
}

// HostLevel returns only the fields of o that reach into the host (mounts,
// devices, capabilities, privileged mode, ulimits, security options and the
// PID namespace) - the group only an Admin may set. Everything else is zero.
func HostLevel(o agentproto.ContainerOptions) agentproto.ContainerOptions {
	return agentproto.ContainerOptions{
		PIDMode:     o.PIDMode,
		Privileged:  o.Privileged,
		Mounts:      o.Mounts,
		Devices:     o.Devices,
		CapAdd:      o.CapAdd,
		CapDrop:     o.CapDrop,
		SecurityOpt: o.SecurityOpt,
		Ulimits:     o.Ulimits,
	}
}

// WithHostLevel returns o with its host-level group replaced by host's. It is
// how a profile edited by someone who may not change that group keeps what an
// Admin set: the form for such a user does not carry those fields at all.
func WithHostLevel(o, host agentproto.ContainerOptions) agentproto.ContainerOptions {
	h := HostLevel(host)
	o.PIDMode = h.PIDMode
	o.Privileged = h.Privileged
	o.Mounts = h.Mounts
	o.Devices = h.Devices
	o.CapAdd = h.CapAdd
	o.CapDrop = h.CapDrop
	o.SecurityOpt = h.SecurityOpt
	o.Ulimits = h.Ulimits
	return o
}

// HostLevelEqual reports whether a and b set the same host-level fields. They
// are compared as JSON so an empty list and an absent one are the same thing.
func HostLevelEqual(a, b agentproto.ContainerOptions) bool {
	ja, errA := json.Marshal(HostLevel(a))
	jb, errB := json.Marshal(HostLevel(b))
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// IsZero reports whether o sets nothing.
func IsZero(o agentproto.ContainerOptions) bool {
	b, err := json.Marshal(o)
	return err == nil && string(b) == "{}"
}

func isEmptyJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}
