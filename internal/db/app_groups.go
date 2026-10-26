// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrAppGroupNotFound is returned when an application group (or, for a
	// membership write, the group or user it names) does not exist.
	ErrAppGroupNotFound = errors.New("application group not found")

	// ErrAppGroupNameTaken is returned when a name is already used by another
	// group, ignoring case.
	ErrAppGroupNameTaken = errors.New("an application group with that name already exists")

	// ErrADGroupAlreadyMapped is returned when an AD group is already mapped to
	// the application group.
	ErrADGroupAlreadyMapped = errors.New("that AD group is already mapped to this application group")
)

// foreignKeyViolationCode is Postgres's SQLSTATE for a foreign key violation.
const foreignKeyViolationCode = "23503"

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolationCode
}

// AppGroup mirrors an app_groups row - see SCHEMA.md Application groups.
type AppGroup struct {
	ID        string
	Name      string
	CreatedBy *string
	CreatedAt time.Time
	UpdatedBy *string
	UpdatedAt time.Time
}

// AppGroupSummary is a group with the counts the Groups list shows.
// MemberCount is how many distinct people are members right now, through any
// mapped AD group's cached membership or by hand.
type AppGroupSummary struct {
	AppGroup
	ADGroupCount     int
	LocalMemberCount int
	MemberCount      int
}

// AppGroupADMapping is one AD group an application group is mapped to.
type AppGroupADMapping struct {
	DN      string
	Name    string
	AddedAt time.Time
}

// AppGroupMember is one person in an application group and why they are in it.
type AppGroupMember struct {
	UserID        string
	DisplayName   string
	Tier          Tier
	LocalUsername *string

	// ViaADGroups names the mapped AD groups that put them here, from the
	// membership cache (so as fresh as their last sign-in or recheck).
	ViaADGroups []string

	// AddedManually is true for a local account an Admin added by hand.
	AddedManually bool
}

// AppGroupRepository is the only component that queries the app_groups,
// app_group_ad_groups, app_group_local_members and user_ad_group_membership
// tables directly.
type AppGroupRepository struct {
	pool *pgxpool.Pool
}

// NewAppGroupRepository wraps an already-established, already-verified pool -
// see New in db.go.
func NewAppGroupRepository(pool *pgxpool.Pool) *AppGroupRepository {
	return &AppGroupRepository{pool: pool}
}

const appGroupColumns = `id, name, created_by, created_at, updated_by, updated_at`

func scanAppGroup(row pgx.Row) (*AppGroup, error) {
	var g AppGroup
	if err := row.Scan(&g.ID, &g.Name, &g.CreatedBy, &g.CreatedAt, &g.UpdatedBy, &g.UpdatedAt); err != nil {
		return nil, err
	}
	return &g, nil
}

// Create adds a group. createdBy is nil only for the break-glass SuperAdmin,
// which is not a Users row.
func (r *AppGroupRepository) Create(ctx context.Context, name string, createdBy *string) (*AppGroup, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO app_groups (name, created_by, updated_by) VALUES ($1, $2, $2) RETURNING `+appGroupColumns,
		name, createdBy)
	g, err := scanAppGroup(row)
	if isUniqueViolation(err) {
		return nil, ErrAppGroupNameTaken
	}
	if err != nil {
		return nil, fmt.Errorf("create application group: %w", err)
	}
	return g, nil
}

// FindByID returns one group, or ErrAppGroupNotFound.
func (r *AppGroupRepository) FindByID(ctx context.Context, id string) (*AppGroup, error) {
	g, err := scanAppGroup(r.pool.QueryRow(ctx, `SELECT `+appGroupColumns+` FROM app_groups WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppGroupNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find application group: %w", err)
	}
	return g, nil
}

// Rename changes a group's name, or returns ErrAppGroupNotFound or
// ErrAppGroupNameTaken.
func (r *AppGroupRepository) Rename(ctx context.Context, id, name string, updatedBy *string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE app_groups SET name = $2, updated_by = $3, updated_at = now() WHERE id = $1::uuid`, id, name, updatedBy)
	if isUniqueViolation(err) {
		return ErrAppGroupNameTaken
	}
	if err != nil {
		return fmt.Errorf("rename application group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAppGroupNotFound
	}
	return nil
}

// Delete removes a group; its mappings, manual members and alert audiences go
// with it. It returns ErrAppGroupNotFound when there is no such group.
func (r *AppGroupRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM app_groups WHERE id = $1::uuid`, id)
	if err != nil {
		return fmt.Errorf("delete application group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAppGroupNotFound
	}
	return nil
}

// ListWithCounts returns every group, A to Z, with its counts.
func (r *AppGroupRepository) ListWithCounts(ctx context.Context) ([]*AppGroupSummary, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT g.id, g.name, g.created_by, g.created_at, g.updated_by, g.updated_at,
		  (SELECT count(*) FROM app_group_ad_groups m WHERE m.app_group_id = g.id),
		  (SELECT count(*) FROM app_group_local_members l WHERE l.app_group_id = g.id),
		  (SELECT count(*) FROM (
		      SELECT l.user_id FROM app_group_local_members l WHERE l.app_group_id = g.id
		      UNION
		      SELECT u.user_id FROM app_group_ad_groups m
		        JOIN user_ad_group_membership u ON lower(u.ad_group_dn) = lower(m.ad_group_dn)
		        WHERE m.app_group_id = g.id
		  ) members)
		FROM app_groups g ORDER BY lower(g.name)`)
	if err != nil {
		return nil, fmt.Errorf("list application groups: %w", err)
	}
	defer rows.Close()
	var out []*AppGroupSummary
	for rows.Next() {
		var s AppGroupSummary
		if err := rows.Scan(&s.ID, &s.Name, &s.CreatedBy, &s.CreatedAt, &s.UpdatedBy, &s.UpdatedAt,
			&s.ADGroupCount, &s.LocalMemberCount, &s.MemberCount); err != nil {
			return nil, fmt.Errorf("scan application group: %w", err)
		}
		out = append(out, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list application groups: %w", err)
	}
	return out, nil
}

// AddADMapping maps an AD group to an application group. dn and name are what
// the directory itself returned. It returns ErrADGroupAlreadyMapped for a
// repeat, or ErrAppGroupNotFound when the application group is gone.
func (r *AppGroupRepository) AddADMapping(ctx context.Context, groupID, dn, name string, addedBy *string) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO app_group_ad_groups (app_group_id, ad_group_dn, ad_group_name, added_by) VALUES ($1::uuid, $2, $3, $4)`,
		groupID, dn, name, addedBy)
	switch {
	case isUniqueViolation(err):
		return ErrADGroupAlreadyMapped
	case isForeignKeyViolation(err):
		return ErrAppGroupNotFound
	case err != nil:
		return fmt.Errorf("map AD group: %w", err)
	}
	return nil
}

// RemoveADMapping unmaps an AD group, comparing DNs ignoring case, and reports
// whether a mapping was removed.
func (r *AppGroupRepository) RemoveADMapping(ctx context.Context, groupID, dn string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM app_group_ad_groups WHERE app_group_id = $1::uuid AND lower(ad_group_dn) = lower($2)`, groupID, dn)
	if err != nil {
		return false, fmt.Errorf("unmap AD group: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListADMappings returns a group's mapped AD groups, A to Z.
func (r *AppGroupRepository) ListADMappings(ctx context.Context, groupID string) ([]AppGroupADMapping, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT ad_group_dn, ad_group_name, added_at FROM app_group_ad_groups
		 WHERE app_group_id = $1::uuid ORDER BY lower(ad_group_name)`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list AD group mappings: %w", err)
	}
	defer rows.Close()
	var out []AppGroupADMapping
	for rows.Next() {
		var m AppGroupADMapping
		if err := rows.Scan(&m.DN, &m.Name, &m.AddedAt); err != nil {
			return nil, fmt.Errorf("scan AD group mapping: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list AD group mappings: %w", err)
	}
	return out, nil
}

// MappedDNs returns every AD group DN any application group is mapped to, once
// each - the only groups the membership cache ever holds.
func (r *AppGroupRepository) MappedDNs(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT DISTINCT ON (lower(ad_group_dn)) ad_group_dn FROM app_group_ad_groups ORDER BY lower(ad_group_dn)`)
	if err != nil {
		return nil, fmt.Errorf("list mapped AD groups: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var dn string
		if err := rows.Scan(&dn); err != nil {
			return nil, fmt.Errorf("scan mapped AD group: %w", err)
		}
		out = append(out, dn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mapped AD groups: %w", err)
	}
	return out, nil
}

// AddLocalMember adds a person to a group by hand and reports whether they
// were newly added (false when they already were). It returns
// ErrAppGroupNotFound when the group or the user does not exist.
func (r *AppGroupRepository) AddLocalMember(ctx context.Context, groupID, userID string, addedBy *string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`INSERT INTO app_group_local_members (app_group_id, user_id, added_by) VALUES ($1::uuid, $2::uuid, $3)
		 ON CONFLICT (app_group_id, user_id) DO NOTHING`, groupID, userID, addedBy)
	if isForeignKeyViolation(err) {
		return false, ErrAppGroupNotFound
	}
	if err != nil {
		return false, fmt.Errorf("add local group member: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RemoveLocalMember removes a manually added member and reports whether they
// were one.
func (r *AppGroupRepository) RemoveLocalMember(ctx context.Context, groupID, userID string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM app_group_local_members WHERE app_group_id = $1::uuid AND user_id = $2::uuid`, groupID, userID)
	if err != nil {
		return false, fmt.Errorf("remove local group member: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Members returns everyone in the group, A to Z by display name, with why.
func (r *AppGroupRepository) Members(ctx context.Context, groupID string) ([]AppGroupMember, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT u.id, u.display_name, u.tier, u.local_username,
		       COALESCE(array_agg(DISTINCT m.ad_group_name) FILTER (WHERE m.app_group_id IS NOT NULL), '{}'),
		       COALESCE(bool_or(l.user_id IS NOT NULL), false)
		FROM users u
		LEFT JOIN app_group_local_members l ON l.user_id = u.id AND l.app_group_id = $1::uuid
		LEFT JOIN user_ad_group_membership c ON c.user_id = u.id
		LEFT JOIN app_group_ad_groups m ON m.app_group_id = $1::uuid AND lower(m.ad_group_dn) = lower(c.ad_group_dn)
		WHERE l.user_id IS NOT NULL OR m.app_group_id IS NOT NULL
		GROUP BY u.id
		ORDER BY lower(u.display_name), u.id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	defer rows.Close()
	var out []AppGroupMember
	for rows.Next() {
		var m AppGroupMember
		if err := rows.Scan(&m.UserID, &m.DisplayName, &m.Tier, &m.LocalUsername, &m.ViaADGroups, &m.AddedManually); err != nil {
			return nil, fmt.Errorf("scan group member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	return out, nil
}

// lowerDistinct lowercases and de-duplicates DNs: AD compares them ignoring
// case, and the cache's unique index is on lower(dn).
func lowerDistinct(dns []string) []string {
	seen := make(map[string]struct{}, len(dns))
	out := make([]string, 0, len(dns))
	for _, dn := range dns {
		l := strings.ToLower(dn)
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out
}

// ReplaceUserMemberships sets which mapped AD groups one person is cached as
// being in: rows for groups not in dns are removed, the rest are added or
// refreshed. An empty dns clears them. The caller passes only DNs the directory
// said the person is in.
func (r *AppGroupRepository) ReplaceUserMemberships(ctx context.Context, userID string, dns []string) error {
	lowered := lowerDistinct(dns)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin replace user memberships: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM user_ad_group_membership WHERE user_id = $1::uuid AND lower(ad_group_dn) <> ALL($2::text[])`,
		userID, lowered); err != nil {
		return fmt.Errorf("remove stale memberships: %w", err)
	}
	if len(lowered) > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_ad_group_membership (user_id, ad_group_dn, checked_at)
			 SELECT $1::uuid, d, now() FROM unnest($2::text[]) AS d
			 ON CONFLICT (user_id, lower(ad_group_dn)) DO UPDATE SET checked_at = now()`,
			userID, lowered); err != nil {
			return fmt.Errorf("record memberships: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit replace user memberships: %w", err)
	}
	return nil
}

// ReplaceMembersOfDN sets which known people are cached as being in one AD
// group: the backfill done when a group is mapped. People not in userIDs lose
// their row for this group; those in it gain or refresh one.
func (r *AppGroupRepository) ReplaceMembersOfDN(ctx context.Context, dn string, userIDs []string) error {
	if userIDs == nil {
		userIDs = []string{}
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin replace group members: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM user_ad_group_membership WHERE lower(ad_group_dn) = lower($1) AND user_id <> ALL($2::uuid[])`,
		dn, userIDs); err != nil {
		return fmt.Errorf("remove stale members: %w", err)
	}
	if len(userIDs) > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_ad_group_membership (user_id, ad_group_dn, checked_at)
			 SELECT u, lower($1), now() FROM unnest($2::uuid[]) AS u
			 ON CONFLICT (user_id, lower(ad_group_dn)) DO UPDATE SET checked_at = now()`,
			dn, userIDs); err != nil {
			return fmt.Errorf("record members: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit replace group members: %w", err)
	}
	return nil
}

// PruneUnmappedMemberships removes cached memberships of AD groups that no
// application group is mapped to any more, so the cache never outlives its
// mappings. It returns how many rows it removed.
func (r *AppGroupRepository) PruneUnmappedMemberships(ctx context.Context) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM user_ad_group_membership c
		 WHERE NOT EXISTS (SELECT 1 FROM app_group_ad_groups m WHERE lower(m.ad_group_dn) = lower(c.ad_group_dn))`)
	if err != nil {
		return 0, fmt.Errorf("prune unmapped memberships: %w", err)
	}
	return tag.RowsAffected(), nil
}
