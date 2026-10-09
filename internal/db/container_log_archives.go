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

// ErrContainerLogArchiveNotFound is returned when a lookup finds no row.
var ErrContainerLogArchiveNotFound = errors.New("container log archive not found")

// ContainerLogArchive mirrors a container_log_archives row without its log
// body - see SCHEMA.md Container log archives. The body is only read by
// FindByID, so a list never pulls megabytes of text.
type ContainerLogArchive struct {
	ID            string
	UploadID      string
	NodeID        string
	NodeName      string
	InstanceID    *string
	ProfileID     *string
	ProfileName   string
	ContainerName string
	ContainerID   string
	Reason        string
	State         string
	ExitCode      *int
	OOMKilled     bool
	StartedAt     *time.Time
	FinishedAt    *time.Time

	// LinesRequested is nil when every line was asked for.
	LinesRequested *int
	LinesKept      int
	Truncated      bool

	// SizeBytes is the log's size before gzip; CompressedBytes is what is
	// stored.
	SizeBytes       int64
	CompressedBytes int64
	CreatedAt       time.Time
}

// NewContainerLogArchive is the input to ContainerLogArchiveRepository.Create.
type NewContainerLogArchive struct {
	UploadID       string
	NodeID         string
	InstanceID     *string
	ProfileID      *string
	ProfileName    string
	ContainerName  string
	ContainerID    string
	Reason         string
	State          string
	ExitCode       *int
	OOMKilled      bool
	StartedAt      *time.Time
	FinishedAt     *time.Time
	LinesRequested *int
	LinesKept      int
	Truncated      bool
	SizeBytes      int64

	// LogGz is the gzip-compressed log.
	LogGz []byte
}

// ContainerLogArchiveRepository is the only component that queries the
// container_log_archives table directly - see CLAUDE.md: the repository layer
// is the only place that accesses the database directly.
type ContainerLogArchiveRepository struct {
	pool *pgxpool.Pool
}

// NewContainerLogArchiveRepository wraps an already-established,
// already-verified pool - see New in db.go.
func NewContainerLogArchiveRepository(pool *pgxpool.Pool) *ContainerLogArchiveRepository {
	return &ContainerLogArchiveRepository{pool: pool}
}

// containerLogArchiveColumns are the non-body columns, read through a join so
// each row carries its node's name.
const containerLogArchiveColumns = `a.id, a.upload_id, a.node_id, n.name, a.instance_id, a.profile_id, a.profile_name,
	a.container_name, a.container_id, a.reason, a.container_state, a.exit_code, a.oom_killed,
	a.started_at, a.finished_at, a.lines_requested, a.lines_kept, a.truncated, a.size_bytes,
	octet_length(a.log_gz), a.created_at`

const containerLogArchiveFrom = ` FROM container_log_archives a JOIN nodes n ON n.id = a.node_id`

func scanContainerLogArchive(row pgx.Row, extra ...any) (*ContainerLogArchive, error) {
	var a ContainerLogArchive
	dest := append([]any{
		&a.ID, &a.UploadID, &a.NodeID, &a.NodeName, &a.InstanceID, &a.ProfileID, &a.ProfileName,
		&a.ContainerName, &a.ContainerID, &a.Reason, &a.State, &a.ExitCode, &a.OOMKilled,
		&a.StartedAt, &a.FinishedAt, &a.LinesRequested, &a.LinesKept, &a.Truncated, &a.SizeBytes,
		&a.CompressedBytes, &a.CreatedAt,
	}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrContainerLogArchiveNotFound
		}
		return nil, fmt.Errorf("scan container log archive: %w", err)
	}
	return &a, nil
}

// Create stores an archive. It is idempotent on UploadID: an upload that is
// already stored returns the existing row unchanged, so an agent that retries
// after a lost confirmation does not store the log twice.
func (r *ContainerLogArchiveRepository) Create(ctx context.Context, in NewContainerLogArchive) (*ContainerLogArchive, error) {
	var id string
	err := r.pool.QueryRow(ctx,
		`INSERT INTO container_log_archives
		   (upload_id, node_id, instance_id, profile_id, profile_name, container_name, container_id, reason,
		    container_state, exit_code, oom_killed, started_at, finished_at, lines_requested, lines_kept,
		    truncated, size_bytes, log_gz)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		 ON CONFLICT (upload_id) DO NOTHING
		 RETURNING id`,
		in.UploadID, in.NodeID, in.InstanceID, in.ProfileID, in.ProfileName, in.ContainerName, in.ContainerID, in.Reason,
		in.State, in.ExitCode, in.OOMKilled, in.StartedAt, in.FinishedAt, in.LinesRequested, in.LinesKept,
		in.Truncated, in.SizeBytes, in.LogGz).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already stored under this upload id.
		return scanContainerLogArchive(r.pool.QueryRow(ctx,
			`SELECT `+containerLogArchiveColumns+containerLogArchiveFrom+` WHERE a.upload_id = $1`, in.UploadID))
	}
	if err != nil {
		return nil, fmt.Errorf("create container log archive: %w", err)
	}
	return r.findMeta(ctx, id)
}

func (r *ContainerLogArchiveRepository) findMeta(ctx context.Context, id string) (*ContainerLogArchive, error) {
	return scanContainerLogArchive(r.pool.QueryRow(ctx,
		`SELECT `+containerLogArchiveColumns+containerLogArchiveFrom+` WHERE a.id = $1`, id))
}

// List returns the newest archives first, without their log bodies, at most
// limit of them.
func (r *ContainerLogArchiveRepository) List(ctx context.Context, limit int) ([]*ContainerLogArchive, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+containerLogArchiveColumns+containerLogArchiveFrom+` ORDER BY a.created_at DESC, a.id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list container log archives: %w", err)
	}
	defer rows.Close()

	var out []*ContainerLogArchive
	for rows.Next() {
		a, err := scanContainerLogArchive(rows)
		if err != nil {
			return nil, fmt.Errorf("list container log archives: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list container log archives: %w", err)
	}
	return out, nil
}

// FindByID returns one archive and its gzip log body. Returns
// ErrContainerLogArchiveNotFound if no row matches. id must be a valid uuid;
// internal/containerlogs checks that before calling, since a malformed id is
// a database error here, not "not found".
func (r *ContainerLogArchiveRepository) FindByID(ctx context.Context, id string) (*ContainerLogArchive, []byte, error) {
	var logGz []byte
	a, err := scanContainerLogArchive(r.pool.QueryRow(ctx,
		`SELECT `+containerLogArchiveColumns+`, a.log_gz`+containerLogArchiveFrom+` WHERE a.id = $1`, id), &logGz)
	if err != nil {
		return nil, nil, err
	}
	return a, logGz, nil
}

// LatestByInstanceIDs returns, for each of instanceIDs that has any archived
// log, the id of its newest one - what the Profiles page links a profile's
// last run to. An instance with no archive is simply absent from the map.
// Every id must be a valid uuid; internal/containerlogs filters them.
func (r *ContainerLogArchiveRepository) LatestByInstanceIDs(ctx context.Context, instanceIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(instanceIDs))
	if len(instanceIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx,
		`SELECT DISTINCT ON (instance_id) instance_id::text, id::text
		 FROM container_log_archives
		 WHERE instance_id = ANY($1::uuid[])
		 ORDER BY instance_id, created_at DESC, id`, instanceIDs)
	if err != nil {
		return nil, fmt.Errorf("latest container log archives by instance: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var instanceID, archiveID string
		if err := rows.Scan(&instanceID, &archiveID); err != nil {
			return nil, fmt.Errorf("latest container log archives by instance: %w", err)
		}
		out[instanceID] = archiveID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("latest container log archives by instance: %w", err)
	}
	return out, nil
}
