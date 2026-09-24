// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NodeNetworkInterface mirrors the node_network_interfaces table - see
// SCHEMA.md Node network interfaces. Current-state answer to "what
// network interfaces does this node have right now", reported in full and
// replaced wholesale on each report (agent/netinfo) - not a time series.
type NodeNetworkInterface struct {
	NodeID        string
	InterfaceName string
	IPAddress     string
	// LinkSpeedMbps is nil when the agent couldn't determine a link speed
	// (a virtual interface, or a driver that doesn't expose one) -
	// "unknown", not zero, so "Fastest" auto-selection never treats an
	// unmeasured interface as the slowest available one by default; it's
	// simply not a Fastest candidate.
	LinkSpeedMbps *int
	ReportedAt    time.Time
}

// NodeNetworkInterfaceRepository is the only component that queries the
// node_network_interfaces table directly - see CLAUDE.md: the repository
// layer is the only place that accesses the database directly.
type NodeNetworkInterfaceRepository struct {
	pool *pgxpool.Pool
}

// NewNodeNetworkInterfaceRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewNodeNetworkInterfaceRepository(pool *pgxpool.Pool) *NodeNetworkInterfaceRepository {
	return &NodeNetworkInterfaceRepository{pool: pool}
}

const nodeNetworkInterfaceColumns = `node_id, interface_name, ip_address, link_speed_mbps, reported_at`

func scanNodeNetworkInterface(row pgx.Row) (*NodeNetworkInterface, error) {
	var iface NodeNetworkInterface
	err := row.Scan(&iface.NodeID, &iface.InterfaceName, &iface.IPAddress, &iface.LinkSpeedMbps, &iface.ReportedAt)
	if err != nil {
		return nil, fmt.Errorf("scan node network interface: %w", err)
	}
	return &iface, nil
}

// ReplaceForNode replaces a node's entire reported interface set in one
// transaction - a wholesale delete-then-insert, not a per-interface
// upsert, matching how the agent itself reports (its full current list on
// every report, not incremental deltas) - see agent/netinfo. An empty
// interfaces slice is valid (leaves the node with none reported) and is
// not treated as an error.
func (r *NodeNetworkInterfaceRepository) ReplaceForNode(ctx context.Context, nodeID string, interfaces []NodeNetworkInterface) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin replace node network interfaces for node %s: %w", nodeID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after a successful commit is a documented no-op

	if _, err := tx.Exec(ctx, `DELETE FROM node_network_interfaces WHERE node_id = $1`, nodeID); err != nil {
		return fmt.Errorf("replace node network interfaces for node %s: %w", nodeID, err)
	}

	for _, iface := range interfaces {
		if _, err := tx.Exec(ctx,
			`INSERT INTO node_network_interfaces (node_id, interface_name, ip_address, link_speed_mbps)
			 VALUES ($1, $2, $3, $4)`,
			nodeID, iface.InterfaceName, iface.IPAddress, iface.LinkSpeedMbps); err != nil {
			return fmt.Errorf("replace node network interfaces for node %s: %w", nodeID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("replace node network interfaces for node %s: %w", nodeID, err)
	}
	return nil
}

// ListByNode returns every interface a node has most recently reported,
// ordered by name - backs the Nodes edit page's interface table and the
// peer-transfer interface picker (internal/nodes.ResolveTransferSource).
func (r *NodeNetworkInterfaceRepository) ListByNode(ctx context.Context, nodeID string) ([]*NodeNetworkInterface, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+nodeNetworkInterfaceColumns+` FROM node_network_interfaces WHERE node_id = $1 ORDER BY interface_name`,
		nodeID)
	if err != nil {
		return nil, fmt.Errorf("list node network interfaces for node %s: %w", nodeID, err)
	}
	defer rows.Close()

	var interfaces []*NodeNetworkInterface
	for rows.Next() {
		iface, err := scanNodeNetworkInterface(rows)
		if err != nil {
			return nil, fmt.Errorf("list node network interfaces for node %s: %w", nodeID, err)
		}
		interfaces = append(interfaces, iface)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list node network interfaces for node %s: %w", nodeID, err)
	}
	return interfaces, nil
}
