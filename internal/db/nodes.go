// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RuntimeBackend mirrors the runtime_backend Postgres enum - see
// migrations/000014_nodes_collapse_runtime_backend.up.sql and SCHEMA.md
// Nodes. Selects how the agent runs the inference engine: inside a
// Docker or Podman container with GPU passthrough via CDI, or
// (RuntimeBackendBareMetal) as a direct child process when passthrough
// isn't viable - e.g. a single-GPU workstation already using that GPU
// for its own host session. Not specific to any particular hardware
// class - a DGX Spark's GB10 GPU, for instance, supports passthrough to
// a container without affecting a display connected to it (NVIDIA's
// supported use case for that hardware), so a Spark can use any of the
// three depending on its own setup - see PLANNING.md's Decisions Log for
// the correction this replaced (node_type/container_runtime previously
// conflated a hardware label with this choice).
type RuntimeBackend string

const (
	RuntimeBackendDocker    RuntimeBackend = "docker"
	RuntimeBackendPodman    RuntimeBackend = "podman"
	RuntimeBackendBareMetal RuntimeBackend = "bare-metal"
)

// AgentStatus mirrors the agent_status Postgres enum - derived from the
// agent's persistent WebSocket connection state (ARCHITECTURE.md Node &
// Fabric Registry), not set directly by anything in this package yet.
type AgentStatus string

const (
	AgentStatusOnline      AgentStatus = "online"
	AgentStatusOffline     AgentStatus = "offline"
	AgentStatusUnreachable AgentStatus = "unreachable"
)

// Node mirrors the nodes table - see SCHEMA.md Nodes. FabricGroupID is
// deliberately absent - see migrations/000005_create_nodes.up.sql.
type Node struct {
	ID              string
	Name            string
	Hostname        string
	IPAddress       string
	RuntimeBackend  RuntimeBackend
	GPUMemoryGB     float64
	CPUMemoryGB     float64
	AgentStatus     AgentStatus
	LastHeartbeatAt *time.Time
	RegisteredBy    *string
	RegisteredAt    time.Time
}

// ErrNodeNotFound is returned when a lookup finds no matching row.
var ErrNodeNotFound = errors.New("node not found")

// NodeRepository is the only component that queries the nodes table
// directly - see CLAUDE.md: the repository layer is the only place that
// accesses the database directly.
type NodeRepository struct {
	pool *pgxpool.Pool
}

// NewNodeRepository wraps an already-established, already-verified pool -
// see New in db.go.
func NewNodeRepository(pool *pgxpool.Pool) *NodeRepository {
	return &NodeRepository{pool: pool}
}

const nodeColumns = `id, name, hostname, ip_address, runtime_backend,
	gpu_memory_gb, cpu_memory_gb, agent_status, last_heartbeat_at, registered_by, registered_at`

func scanNode(row pgx.Row) (*Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.Name, &n.Hostname, &n.IPAddress, &n.RuntimeBackend,
		&n.GPUMemoryGB, &n.CPUMemoryGB, &n.AgentStatus, &n.LastHeartbeatAt, &n.RegisteredBy, &n.RegisteredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNodeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan node: %w", err)
	}
	return &n, nil
}

// Create inserts a new node. registeredBy is nil only for the break-glass
// SuperAdmin, which is not a Users row - see SCHEMA.md Break-glass
// credential. bearerTokenHash must already be hashed
// (internal/auth.HashNodeToken) - this package never sees the plaintext
// token, same as it never sees a plaintext password. It is deliberately
// excluded from nodeColumns/Node, so it never flows out through FindByID
// or List.
func (r *NodeRepository) Create(ctx context.Context, name, hostname, ipAddress string, runtimeBackend RuntimeBackend, gpuMemoryGB, cpuMemoryGB float64, registeredBy *string, bearerTokenHash string) (*Node, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO nodes (name, hostname, ip_address, runtime_backend, gpu_memory_gb, cpu_memory_gb, registered_by, bearer_token_hash)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING `+nodeColumns,
		name, hostname, ipAddress, runtimeBackend, gpuMemoryGB, cpuMemoryGB, registeredBy, bearerTokenHash)

	n, err := scanNode(row)
	if err != nil {
		return nil, fmt.Errorf("create node: %w", err)
	}
	return n, nil
}

// FindByID looks up a node by its internal ID. Returns ErrNodeNotFound if
// no row matches.
func (r *NodeRepository) FindByID(ctx context.Context, id string) (*Node, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = $1`, id)
	return scanNode(row)
}

// NodeCredential pairs a node's identity with its bearer token hash - the
// only place bearer_token_hash is ever read, and only for connect-time
// verification (internal/nodes.AuthService). It is not part of Node/
// nodeColumns, so the hash can never flow out through FindByID or List.
type NodeCredential struct {
	Node            Node
	BearerTokenHash string
}

// FindCredentialByName looks up a node by its registered name (matching
// SPARKY_NODE_NAME, per docs/AGENT.md Configuration) along with its
// bearer token hash. Returns ErrNodeNotFound if no row matches.
func (r *NodeRepository) FindCredentialByName(ctx context.Context, name string) (*NodeCredential, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+nodeColumns+`, bearer_token_hash FROM nodes WHERE name = $1`, name)

	var c NodeCredential
	err := row.Scan(&c.Node.ID, &c.Node.Name, &c.Node.Hostname, &c.Node.IPAddress, &c.Node.RuntimeBackend,
		&c.Node.GPUMemoryGB, &c.Node.CPUMemoryGB, &c.Node.AgentStatus, &c.Node.LastHeartbeatAt, &c.Node.RegisteredBy, &c.Node.RegisteredAt,
		&c.BearerTokenHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNodeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find node credential: %w", err)
	}
	return &c, nil
}

// SetAgentStatus updates a node's agent_status as its WebSocket connection
// lifecycle changes (internal/agentconn) - see SCHEMA.md Nodes'
// agent_status. bumpHeartbeat also sets last_heartbeat_at to now(): true
// on a successful connect (we just confirmed the agent is alive), false
// on disconnect (last_heartbeat_at should keep recording the last time it
// was actually seen, not the moment it went away).
func (r *NodeRepository) SetAgentStatus(ctx context.Context, nodeID string, status AgentStatus, bumpHeartbeat bool) error {
	var err error
	if bumpHeartbeat {
		_, err = r.pool.Exec(ctx, `UPDATE nodes SET agent_status = $1, last_heartbeat_at = now() WHERE id = $2`, status, nodeID)
	} else {
		_, err = r.pool.Exec(ctx, `UPDATE nodes SET agent_status = $1 WHERE id = $2`, status, nodeID)
	}
	if err != nil {
		return fmt.Errorf("set agent status for node %s: %w", nodeID, err)
	}
	return nil
}

// List returns every registered node, ordered by name - the registry's
// full inventory, for the future Nodes dashboard page.
func (r *NodeRepository) List(ctx context.Context) ([]*Node, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+nodeColumns+` FROM nodes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()

	var nodes []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("list nodes: %w", err)
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	return nodes, nil
}
