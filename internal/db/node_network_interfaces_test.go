// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
)

func TestNodeNetworkInterfaceRepository_ReplaceForNode_InsertsAll(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	interfaces := NewNodeNetworkInterfaceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	speed := 10000

	err := interfaces.ReplaceForNode(ctx, node.ID, []NodeNetworkInterface{
		{InterfaceName: "eth0", IPAddress: "10.0.0.1", LinkSpeedMbps: &speed},
		{InterfaceName: "eth1", IPAddress: "10.0.0.2"}, // unknown speed - nil, not 0
	})
	if err != nil {
		t.Fatalf("ReplaceForNode() error: %v", err)
	}

	got, err := interfaces.ListByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListByNode() error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByNode() returned %d interfaces, want 2", len(got))
	}
	if got[0].InterfaceName != "eth0" || got[0].LinkSpeedMbps == nil || *got[0].LinkSpeedMbps != speed {
		t.Errorf("got[0] = %+v, want eth0 with LinkSpeedMbps=%d", got[0], speed)
	}
	if got[1].InterfaceName != "eth1" || got[1].LinkSpeedMbps != nil {
		t.Errorf("got[1] = %+v, want eth1 with LinkSpeedMbps=nil (unknown, not zero)", got[1])
	}
}

// TestNodeNetworkInterfaceRepository_ReplaceForNode_ReplacesWholesale is
// the actual point of ReplaceForNode over a per-interface upsert: an
// interface the agent no longer reports (unplugged, renamed) must
// disappear, not linger as stale data forever.
func TestNodeNetworkInterfaceRepository_ReplaceForNode_ReplacesWholesale(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	interfaces := NewNodeNetworkInterfaceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	if err := interfaces.ReplaceForNode(ctx, node.ID, []NodeNetworkInterface{
		{InterfaceName: "eth0", IPAddress: "10.0.0.1"},
		{InterfaceName: "eth1", IPAddress: "10.0.0.2"},
	}); err != nil {
		t.Fatalf("first ReplaceForNode() error: %v", err)
	}

	// eth1 is gone from this second report (e.g. unplugged); eth2 is new.
	if err := interfaces.ReplaceForNode(ctx, node.ID, []NodeNetworkInterface{
		{InterfaceName: "eth0", IPAddress: "10.0.0.1"},
		{InterfaceName: "eth2", IPAddress: "10.0.0.3"},
	}); err != nil {
		t.Fatalf("second ReplaceForNode() error: %v", err)
	}

	got, err := interfaces.ListByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListByNode() error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByNode() returned %d interfaces, want 2", len(got))
	}
	names := map[string]bool{}
	for _, iface := range got {
		names[iface.InterfaceName] = true
	}
	if names["eth1"] {
		t.Error("eth1 is still present after a report that no longer includes it")
	}
	if !names["eth0"] || !names["eth2"] {
		t.Errorf("got names = %v, want exactly {eth0, eth2}", names)
	}
}

func TestNodeNetworkInterfaceRepository_ReplaceForNode_EmptyClearsAll(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	interfaces := NewNodeNetworkInterfaceRepository(pool)
	ctx := context.Background()

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	if err := interfaces.ReplaceForNode(ctx, node.ID, []NodeNetworkInterface{
		{InterfaceName: "eth0", IPAddress: "10.0.0.1"},
	}); err != nil {
		t.Fatalf("first ReplaceForNode() error: %v", err)
	}

	if err := interfaces.ReplaceForNode(ctx, node.ID, nil); err != nil {
		t.Fatalf("ReplaceForNode(nil) error: %v", err)
	}

	got, err := interfaces.ListByNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListByNode() error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListByNode() returned %d interfaces, want 0 after an empty report", len(got))
	}
}

func TestNodeNetworkInterfaceRepository_ListByNode_Empty(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	interfaces := NewNodeNetworkInterfaceRepository(pool)

	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))

	got, err := interfaces.ListByNode(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("ListByNode() error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListByNode() returned %d interfaces, want 0 for a node that has never reported", len(got))
	}
}
