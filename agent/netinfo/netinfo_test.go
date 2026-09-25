// SPDX-License-Identifier: AGPL-3.0-or-later

package netinfo

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func cidr(t *testing.T, s string) net.Addr {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	n.IP = ip
	return n
}

func TestList(t *testing.T) {
	sys := t.TempDir()
	writeSpeed := func(name, v string) {
		if err := os.MkdirAll(filepath.Join(sys, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sys, name, "speed"), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSpeed("eth0", "10000\n")
	writeSpeed("eth1", "-1\n")

	ifaces := []net.Interface{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
		{Name: "eth0", Flags: net.FlagUp},
		{Name: "eth1", Flags: net.FlagUp},
		{Name: "eth2", Flags: 0},
		{Name: "docker0", Flags: net.FlagUp},
		{Name: "eth3", Flags: net.FlagUp},
		{Name: "wlan0", Flags: net.FlagUp},
	}
	addrs := map[string][]net.Addr{
		"lo":      {cidr(t, "127.0.0.1/8")},
		"eth0":    {cidr(t, "fe80::1/64"), cidr(t, "10.0.0.5/24")},
		"eth1":    {cidr(t, "192.168.1.5/24")},
		"eth2":    {cidr(t, "10.9.9.9/24")},
		"docker0": {cidr(t, "172.17.0.1/16")},
		"eth3":    {cidr(t, "169.254.1.1/16")},
		"wlan0":   {cidr(t, "2001:db8::5/64")},
	}
	got, err := list(func() ([]net.Interface, error) { return ifaces, nil },
		func(i net.Interface) ([]net.Addr, error) { return addrs[i.Name], nil }, sys)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Fatalf("got %+v, want eth0, eth1, wlan0 only", got)
	}
	if got[0].Name != "eth0" || got[0].IPAddress != "10.0.0.5" || got[0].LinkSpeedMbps == nil || *got[0].LinkSpeedMbps != 10000 {
		t.Errorf("eth0 = %+v", got[0])
	}
	if got[1].Name != "eth1" || got[1].LinkSpeedMbps != nil {
		t.Errorf("eth1 (speed -1) = %+v, want nil speed", got[1])
	}
	if got[2].Name != "wlan0" || got[2].IPAddress != "2001:db8::5" || got[2].LinkSpeedMbps != nil {
		t.Errorf("wlan0 (no speed file, IPv6 only) = %+v", got[2])
	}
}
