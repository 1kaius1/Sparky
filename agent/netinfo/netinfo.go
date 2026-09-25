// SPDX-License-Identifier: AGPL-3.0-or-later

// Package netinfo enumerates this node's network interfaces and their link
// speeds, for peer-to-peer model transfer's network-path selection (SCHEMA.md
// Node network interfaces). Link speed is read straight from sysfs rather
// than shelling out to ethtool, avoiding a new mandatory host binary -
// same preference agent/telemetry already shows for /proc over heavier
// tooling.
package netinfo

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Interface is one usable network interface. LinkSpeedMbps is nil when the
// speed can't be determined (a virtual interface, or a driver that doesn't
// expose one) - "unknown", never 0, so it is never mistaken for "slowest".
type Interface struct {
	Name          string
	IPAddress     string
	LinkSpeedMbps *int
}

const sysClassNet = "/sys/class/net"

// virtualPrefixes are container/VM plumbing interfaces that are never a
// sensible path for a model transfer between hosts.
var virtualPrefixes = []string{"docker", "veth", "br-", "virbr", "cni", "flannel", "cali", "lo"}

// List returns every up, non-loopback interface that has a usable address.
func List() ([]Interface, error) {
	return list(net.Interfaces, func(i net.Interface) ([]net.Addr, error) { return i.Addrs() }, sysClassNet)
}

func list(interfaces func() ([]net.Interface, error), addrsOf func(net.Interface) ([]net.Addr, error), sysRoot string) ([]Interface, error) {
	all, err := interfaces()
	if err != nil {
		return nil, err
	}
	var out []Interface
	for _, ifc := range all {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || isVirtual(ifc.Name) {
			continue
		}
		addrs, err := addrsOf(ifc)
		if err != nil {
			continue
		}
		ip := pickAddress(addrs)
		if ip == "" {
			continue
		}
		out = append(out, Interface{Name: ifc.Name, IPAddress: ip, LinkSpeedMbps: linkSpeed(sysRoot, ifc.Name)})
	}
	return out, nil
}

func isVirtual(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// pickAddress prefers a global IPv4 address, falling back to a global IPv6
// one. Link-local addresses are skipped - a peer can't reliably dial them
// without a zone.
func pickAddress(addrs []net.Addr) string {
	var v6 string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || !ipnet.IP.IsGlobalUnicast() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
		if v6 == "" {
			v6 = ipnet.IP.String()
		}
	}
	return v6
}

// linkSpeed reads <sysRoot>/<name>/speed. The kernel reports -1 (or the
// read fails outright) for an interface with no meaningful speed - both
// map to nil, not 0.
func linkSpeed(sysRoot, name string) *int {
	raw, err := os.ReadFile(filepath.Join(sysRoot, name, "speed"))
	if err != nil {
		return nil
	}
	mbps, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || mbps <= 0 {
		return nil
	}
	return &mbps
}
