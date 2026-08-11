// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProfileTopology mirrors the profile_topology Postgres enum - see
// migrations/000007_create_model_profiles.up.sql and SCHEMA.md Model
// profiles. Only ProfileTopologySingleNode is actually creatable in
// v0.1.0 - the database's model_profiles_single_node_only CHECK
// constraint enforces this regardless of caller discipline.
type ProfileTopology string

const (
	ProfileTopologySingleNode ProfileTopology = "single_node"
	ProfileTopologyClustered  ProfileTopology = "clustered"
)

// ProfileEngineType mirrors the profile_engine_type Postgres enum -
// selects the engine adapter (internal/engines) that validates a
// profile's EngineParams. ProfileEngineAphrodite has no adapter until
// v0.3.0 - see PLANNING.md's Model profiles phase breakdown.
type ProfileEngineType string

const (
	ProfileEngineVLLM      ProfileEngineType = "vllm"
	ProfileEngineAphrodite ProfileEngineType = "aphrodite"
	ProfileEngineLlamaCPP  ProfileEngineType = "llamacpp"
)

// Profile mirrors the model_profiles table - see SCHEMA.md Model
// profiles. FabricGroupID is deliberately absent - see
// migrations/000007_create_model_profiles.up.sql.
type Profile struct {
	ID                       string
	Name                     string
	ModelRef                 string
	EngineType               ProfileEngineType
	EngineParams             json.RawMessage
	RequiresFullGPUResidency bool
	RequiredMemoryGB         *float64
	Topology                 ProfileTopology
	TargetNodeID             *string
	Port                     int
	CreatedBy                *string
	CreatedAt                time.Time
	UpdatedBy                *string
	UpdatedAt                time.Time
}

// ErrProfileNotFound is returned when a lookup, update, or delete finds
// no matching row.
var ErrProfileNotFound = errors.New("model profile not found")

// ProfileRepository is the only component that queries the
// model_profiles table directly - see CLAUDE.md: the repository layer
// is the only place that accesses the database directly.
type ProfileRepository struct {
	pool *pgxpool.Pool
}

// NewProfileRepository wraps an already-established, already-verified
// pool - see New in db.go.
func NewProfileRepository(pool *pgxpool.Pool) *ProfileRepository {
	return &ProfileRepository{pool: pool}
}

const profileColumns = `id, name, model_ref, engine_type, engine_params, requires_full_gpu_residency,
	required_memory_gb, topology, target_node_id, port, created_by, created_at, updated_by, updated_at`

func scanProfile(row pgx.Row) (*Profile, error) {
	var p Profile
	err := row.Scan(&p.ID, &p.Name, &p.ModelRef, &p.EngineType, &p.EngineParams, &p.RequiresFullGPUResidency,
		&p.RequiredMemoryGB, &p.Topology, &p.TargetNodeID, &p.Port, &p.CreatedBy, &p.CreatedAt, &p.UpdatedBy, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan model profile: %w", err)
	}
	return &p, nil
}

// Create inserts a new model profile. Every v0.1.0 profile is
// single-node - topology is set by the column default
// (ProfileTopologySingleNode) and targetNodeID is required, matching
// the database's model_profiles_single_node_only CHECK constraint.
// createdBy is nil only for the break-glass SuperAdmin, which is not a
// Users row - see SCHEMA.md Break-glass credential.
func (r *ProfileRepository) Create(ctx context.Context, name, modelRef string, engineType ProfileEngineType, engineParams json.RawMessage, requiresFullGPUResidency bool, requiredMemoryGB *float64, targetNodeID string, port int, createdBy *string) (*Profile, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO model_profiles (name, model_ref, engine_type, engine_params, requires_full_gpu_residency, required_memory_gb, target_node_id, port, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING `+profileColumns,
		name, modelRef, engineType, engineParams, requiresFullGPUResidency, requiredMemoryGB, targetNodeID, port, createdBy)

	p, err := scanProfile(row)
	if err != nil {
		return nil, fmt.Errorf("create model profile: %w", err)
	}
	return p, nil
}

// FindByID looks up a profile by its internal ID. Returns
// ErrProfileNotFound if no row matches.
func (r *ProfileRepository) FindByID(ctx context.Context, id string) (*Profile, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+profileColumns+` FROM model_profiles WHERE id = $1`, id)
	return scanProfile(row)
}

// List returns every model profile, ordered by name - the registry's
// full inventory, for the future Model profiles dashboard page.
func (r *ProfileRepository) List(ctx context.Context) ([]*Profile, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+profileColumns+` FROM model_profiles ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list model profiles: %w", err)
	}
	defer rows.Close()

	var profiles []*Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("list model profiles: %w", err)
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list model profiles: %w", err)
	}
	return profiles, nil
}

// Update replaces a profile's mutable fields. Topology is not
// updatable in v0.1.0 - every profile stays single-node, per
// migrations/000007_create_model_profiles.up.sql's CHECK constraint.
// updatedBy is nil only for the break-glass SuperAdmin. Returns
// ErrProfileNotFound if no row matches id.
func (r *ProfileRepository) Update(ctx context.Context, id, name, modelRef string, engineType ProfileEngineType, engineParams json.RawMessage, requiresFullGPUResidency bool, requiredMemoryGB *float64, targetNodeID string, port int, updatedBy *string) (*Profile, error) {
	row := r.pool.QueryRow(ctx,
		`UPDATE model_profiles
		 SET name = $2, model_ref = $3, engine_type = $4, engine_params = $5, requires_full_gpu_residency = $6,
		     required_memory_gb = $7, target_node_id = $8, port = $9, updated_by = $10, updated_at = now()
		 WHERE id = $1
		 RETURNING `+profileColumns,
		id, name, modelRef, engineType, engineParams, requiresFullGPUResidency, requiredMemoryGB, targetNodeID, port, updatedBy)

	// Not wrapped: ErrProfileNotFound (id doesn't exist) is an expected,
	// common outcome here, not an edge case - callers compare against it
	// directly, same as FindByID.
	return scanProfile(row)
}

// Delete removes a profile by ID. Returns ErrProfileNotFound if no row
// matches - a hard delete, since nothing references model_profiles yet
// (Running instances, a later v0.1.0 item, does not exist at this
// point).
func (r *ProfileRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM model_profiles WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete model profile %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProfileNotFound
	}
	return nil
}
