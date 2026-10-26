// SPDX-License-Identifier: AGPL-3.0-or-later

// Package groups manages application groups: named groups of people that one
// or more AD groups are mapped to, plus local-only accounts an Admin adds by
// hand. See SCHEMA.md Application groups and PLANNING.md's 2026-10-26
// Decisions Log for the design.
//
// A person is in an application group when the membership cache says they are
// in any AD group mapped to it, or when they are a local account added by
// hand. The cache holds only mapped AD groups and is refreshed at login and at
// the hourly session recheck (RefreshUser), and once when a group is mapped
// (the backfill), so a new mapping shows members at once. Every change a person
// makes is audited; the cache refresh is derived data and is not.
//
// The directory is optional: a deployment with no AD has no GroupDirectory,
// and groups then work with manual membership alone.
package groups

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/1kaius1/Sparky/internal/auth"
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

const (
	// MaxNameLen is the longest group name, in characters.
	MaxNameLen = 64

	// MinSearchLen and MaxSearchLen bound a directory search term. A term this
	// short would list a large part of the directory, which the search is
	// deliberately not for.
	MinSearchLen = 3
	MaxSearchLen = 64

	// SearchLimit is how many directory groups one search returns at most.
	SearchLimit = 25
)

var (
	// ErrNotFound is returned for a group (or a member) that does not exist.
	ErrNotFound = errors.New("application group not found")

	// ErrNameTaken is returned when another group already has the name,
	// ignoring case.
	ErrNameTaken = errors.New("an application group with that name already exists")

	// ErrInvalidName is returned for a name that breaks the naming rules.
	ErrInvalidName = errors.New("invalid group name")

	// ErrInvalidSearch is returned for a directory search term that is too
	// short, too long or has control characters.
	ErrInvalidSearch = errors.New("invalid search")

	// ErrDirectoryUnavailable is returned when the deployment has no AD
	// configured, so there is nothing to search or map.
	ErrDirectoryUnavailable = errors.New("no directory is configured")

	// ErrADGroupNotFound is returned when the directory has no such group.
	ErrADGroupNotFound = errors.New("that group was not found in the directory")

	// ErrDirectory is returned when the directory could not be reached or did
	// not answer. The wrapped text has the detail, for the log; callers show
	// people a fixed message, never that detail.
	ErrDirectory = errors.New("the directory could not be reached or did not answer")

	// ErrAlreadyMapped is returned when the AD group is already mapped to the
	// application group.
	ErrAlreadyMapped = errors.New("that AD group is already mapped to this group")

	// ErrNotLocalAccount is returned when a manual member is not a local-only
	// account. An AD account's membership comes from AD alone.
	ErrNotLocalAccount = errors.New("only local accounts can be added by hand; an AD account is a member through its AD groups")
)

// store is the subset of *db.AppGroupRepository this package needs.
type store interface {
	Create(ctx context.Context, name string, createdBy *string) (*db.AppGroup, error)
	FindByID(ctx context.Context, id string) (*db.AppGroup, error)
	Rename(ctx context.Context, id, name string, updatedBy *string) error
	Delete(ctx context.Context, id string) error
	ListWithCounts(ctx context.Context) ([]*db.AppGroupSummary, error)
	AddADMapping(ctx context.Context, groupID, dn, name string, addedBy *string) error
	RemoveADMapping(ctx context.Context, groupID, dn string) (bool, error)
	ListADMappings(ctx context.Context, groupID string) ([]db.AppGroupADMapping, error)
	MappedDNs(ctx context.Context) ([]string, error)
	AddLocalMember(ctx context.Context, groupID, userID string, addedBy *string) (bool, error)
	RemoveLocalMember(ctx context.Context, groupID, userID string) (bool, error)
	Members(ctx context.Context, groupID string) ([]db.AppGroupMember, error)
	ReplaceUserMemberships(ctx context.Context, userID string, dns []string) error
	ReplaceMembersOfDN(ctx context.Context, dn string, userIDs []string) error
	PruneUnmappedMemberships(ctx context.Context) (int64, error)
}

// userStore is the subset of *db.UserRepository this package needs.
type userStore interface {
	FindByID(ctx context.Context, id string) (*db.User, error)
	List(ctx context.Context) ([]*db.User, error)
	FindIDsByLDAPDNs(ctx context.Context, dns []string) ([]string, error)
}

// auditRecorder is the subset of *audit.Recorder this package needs.
type auditRecorder interface {
	Record(ctx context.Context, actorID *string, isSuperAdminAction bool, action, objectType, objectID string, detail map[string]any) error
}

// Service manages application groups and their membership.
type Service struct {
	store     store
	users     userStore
	directory auth.GroupDirectory // nil when no AD is configured
	audit     auditRecorder
	logger    *log.Logger
}

// NewService constructs a Service. directory is nil for a deployment without
// AD (the only way to be a member is then to be added by hand).
func NewService(store store, users userStore, directory auth.GroupDirectory, audit auditRecorder, logger *log.Logger) *Service {
	return &Service{store: store, users: users, directory: directory, audit: audit, logger: logger}
}

// DirectoryAvailable reports whether AD groups can be searched and mapped.
func (s *Service) DirectoryAvailable() bool { return s.directory != nil }

func validateName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("%w: a name is required", ErrInvalidName)
	}
	if n := utf8.RuneCountInString(name); n > MaxNameLen {
		return "", fmt.Errorf("%w: at most %d characters", ErrInvalidName, MaxNameLen)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: no control characters", ErrInvalidName)
		}
	}
	return name, nil
}

func (s *Service) record(ctx context.Context, actor rbac.Actor, action, groupID string, detail map[string]any) error {
	var actorID *string
	if !actor.IsSuperAdmin {
		actorID = &actor.UserID
	}
	return s.audit.Record(ctx, actorID, actor.IsSuperAdmin, action, "app_group", groupID, detail)
}

func actorID(actor rbac.Actor) *string {
	if actor.IsSuperAdmin {
		return nil
	}
	return &actor.UserID
}

// List returns every group with its counts. Requires rbac.CanManageGroups.
// Viewing is not audited.
func (s *Service) List(ctx context.Context, actor rbac.Actor) ([]*db.AppGroupSummary, error) {
	if !rbac.CanManageGroups(actor) {
		return nil, rbac.ErrNotPermitted
	}
	list, err := s.store.ListWithCounts(ctx)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// Detail is one group for its page.
type Detail struct {
	Group    *db.AppGroup
	Mappings []db.AppGroupADMapping
	Members  []db.AppGroupMember

	// DirectoryAvailable is false when no AD is configured: the search and
	// the Map controls are then not offered.
	DirectoryAvailable bool

	// CanManageLocalMembers is whether the viewer may add or remove manual
	// members; AddableLocalUsers (only filled when it is true) are the local
	// accounts that are not yet manual members.
	CanManageLocalMembers bool
	AddableLocalUsers     []*db.User
}

// Get returns one group's page. Requires rbac.CanManageGroups.
func (s *Service) Get(ctx context.Context, actor rbac.Actor, id string) (*Detail, error) {
	if !rbac.CanManageGroups(actor) {
		return nil, rbac.ErrNotPermitted
	}
	g, err := s.store.FindByID(ctx, id)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	mappings, err := s.store.ListADMappings(ctx, id)
	if err != nil {
		return nil, err
	}
	members, err := s.store.Members(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &Detail{
		Group: g, Mappings: mappings, Members: members,
		DirectoryAvailable:    s.directory != nil,
		CanManageLocalMembers: rbac.CanManageGroupLocalMembers(actor),
	}
	if d.CanManageLocalMembers {
		manual := make(map[string]bool, len(members))
		for _, m := range members {
			if m.AddedManually {
				manual[m.UserID] = true
			}
		}
		all, err := s.users.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list users: %w", err)
		}
		for _, u := range all {
			if u.LocalUsername != nil && !manual[u.ID] {
				d.AddableLocalUsers = append(d.AddableLocalUsers, u)
			}
		}
	}
	return d, nil
}

func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, db.ErrAppGroupNotFound):
		return ErrNotFound
	case errors.Is(err, db.ErrAppGroupNameTaken):
		return ErrNameTaken
	case errors.Is(err, db.ErrADGroupAlreadyMapped):
		return ErrAlreadyMapped
	default:
		return err
	}
}

// Create adds a group. Requires rbac.CanManageGroups. Audited as
// created_app_group; a failed audit write removes the group again.
func (s *Service) Create(ctx context.Context, actor rbac.Actor, rawName string) (*db.AppGroup, error) {
	if !rbac.CanManageGroups(actor) {
		return nil, rbac.ErrNotPermitted
	}
	name, err := validateName(rawName)
	if err != nil {
		return nil, err
	}
	g, err := s.store.Create(ctx, name, actorID(actor))
	if err != nil {
		return nil, mapStoreErr(err)
	}
	if err := s.record(ctx, actor, "created_app_group", g.ID, map[string]any{"name": g.Name}); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.store.Delete(ctx, g.ID); revertErr != nil {
			return nil, fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return nil, auditErr
	}
	return g, nil
}

// Rename changes a group's name. Requires rbac.CanManageGroups. Audited as
// renamed_app_group; a failed audit write puts the old name back.
func (s *Service) Rename(ctx context.Context, actor rbac.Actor, id, rawName string) error {
	if !rbac.CanManageGroups(actor) {
		return rbac.ErrNotPermitted
	}
	name, err := validateName(rawName)
	if err != nil {
		return err
	}
	old, err := s.store.FindByID(ctx, id)
	if err != nil {
		return mapStoreErr(err)
	}
	if old.Name == name {
		return nil
	}
	if err := s.store.Rename(ctx, id, name, actorID(actor)); err != nil {
		return mapStoreErr(err)
	}
	if err := s.record(ctx, actor, "renamed_app_group", id, map[string]any{"old_name": old.Name, "new_name": name}); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.store.Rename(ctx, id, old.Name, old.UpdatedBy); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}

// Delete removes a group, its AD mappings, its manual members, and it from the
// audiences of any alert that named it. Requires rbac.CanManageGroups. Audited
// as deleted_app_group after it persists, like the other deletes in this
// project (a deletion cannot be put back if its audit write fails).
func (s *Service) Delete(ctx context.Context, actor rbac.Actor, id string) error {
	if !rbac.CanManageGroups(actor) {
		return rbac.ErrNotPermitted
	}
	g, err := s.store.FindByID(ctx, id)
	if err != nil {
		return mapStoreErr(err)
	}
	mappings, err := s.store.ListADMappings(ctx, id)
	if err != nil {
		return err
	}
	members, err := s.store.Members(ctx, id)
	if err != nil {
		return err
	}
	manual := 0
	for _, m := range members {
		if m.AddedManually {
			manual++
		}
	}
	if err := s.store.Delete(ctx, id); err != nil {
		return mapStoreErr(err)
	}
	// The cache must not outlive the mappings that justified it.
	if _, err := s.store.PruneUnmappedMemberships(ctx); err != nil {
		s.logger.Printf("groups: prune memberships after deleting group %s: %v", id, err)
	}
	detail := map[string]any{"name": g.Name, "ad_groups": len(mappings), "manual_members": manual}
	if err := s.record(ctx, actor, "deleted_app_group", id, detail); err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

// Candidate is a directory group found by a search, and whether it is already
// mapped to the group being edited.
type Candidate struct {
	auth.DirectoryGroup
	Mapped bool
}

// SearchResult is the outcome of a directory search.
type SearchResult struct {
	Candidates []Candidate
	Truncated  bool
}

func validateSearch(raw string) (string, error) {
	term := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(term)
	if n < MinSearchLen {
		return "", fmt.Errorf("%w: type at least %d characters of the group's name", ErrInvalidSearch, MinSearchLen)
	}
	if n > MaxSearchLen {
		return "", fmt.Errorf("%w: at most %d characters", ErrInvalidSearch, MaxSearchLen)
	}
	for _, r := range term {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: no control characters", ErrInvalidSearch)
		}
	}
	return term, nil
}

// SearchDirectory finds AD groups by name for the group being edited, marking
// those already mapped. Requires rbac.CanManageGroups. Nothing is listed
// unprompted: a short or empty term is refused. A read, so not audited.
func (s *Service) SearchDirectory(ctx context.Context, actor rbac.Actor, groupID, rawTerm string) (*SearchResult, error) {
	if !rbac.CanManageGroups(actor) {
		return nil, rbac.ErrNotPermitted
	}
	if s.directory == nil {
		return nil, ErrDirectoryUnavailable
	}
	term, err := validateSearch(rawTerm)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.FindByID(ctx, groupID); err != nil {
		return nil, mapStoreErr(err)
	}
	mappings, err := s.store.ListADMappings(ctx, groupID)
	if err != nil {
		return nil, err
	}
	mapped := make(map[string]bool, len(mappings))
	for _, m := range mappings {
		mapped[strings.ToLower(m.DN)] = true
	}

	found, err := s.directory.SearchGroups(ctx, term, SearchLimit)
	if err != nil {
		return nil, fmt.Errorf("%w: search the directory: %v", ErrDirectory, err)
	}
	out := &SearchResult{Truncated: found.Truncated}
	for _, g := range found.Groups {
		out.Candidates = append(out.Candidates, Candidate{DirectoryGroup: g, Mapped: mapped[strings.ToLower(g.DN)]})
	}
	return out, nil
}

// MapResult is what mapping an AD group did. BackfillFailed is true when the
// mapping was made but the directory's member list could not be read; members
// then appear as people sign in or are rechecked.
type MapResult struct {
	Group          auth.DirectoryGroup
	MatchedMembers int
	BackfillFailed bool
}

// MapADGroup maps an AD group to an application group. The server re-fetches
// the group from the directory and stores the directory's own DN and name,
// never what the browser posted. It then asks the directory for the group's
// members once and caches those it already knows (the backfill), so the
// members show at once. Requires rbac.CanManageGroups. Audited as
// mapped_ad_group; a failed audit write unmaps it again.
func (s *Service) MapADGroup(ctx context.Context, actor rbac.Actor, groupID, dn string) (*MapResult, error) {
	if !rbac.CanManageGroups(actor) {
		return nil, rbac.ErrNotPermitted
	}
	if s.directory == nil {
		return nil, ErrDirectoryUnavailable
	}
	g, err := s.store.FindByID(ctx, groupID)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	found, err := s.directory.FindGroup(ctx, dn)
	if errors.Is(err, auth.ErrGroupNotFound) {
		return nil, ErrADGroupNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: look up the group: %v", ErrDirectory, err)
	}
	if err := s.store.AddADMapping(ctx, groupID, found.DN, found.Name, actorID(actor)); err != nil {
		return nil, mapStoreErr(err)
	}

	res := &MapResult{Group: *found}
	if memberDNs, err := s.directory.GroupMemberDNs(ctx, found.DN); err != nil {
		s.logger.Printf("groups: list members of %q to backfill: %v", found.DN, err)
		res.BackfillFailed = true
	} else if ids, err := s.users.FindIDsByLDAPDNs(ctx, memberDNs); err != nil {
		s.logger.Printf("groups: match members of %q to known users: %v", found.DN, err)
		res.BackfillFailed = true
	} else if err := s.store.ReplaceMembersOfDN(ctx, found.DN, ids); err != nil {
		s.logger.Printf("groups: cache members of %q: %v", found.DN, err)
		res.BackfillFailed = true
	} else {
		res.MatchedMembers = len(ids)
	}

	detail := map[string]any{
		"group_name": g.Name, "ad_group_dn": found.DN, "ad_group_name": found.Name,
		"matched_members": res.MatchedMembers, "backfill_failed": res.BackfillFailed,
	}
	if err := s.record(ctx, actor, "mapped_ad_group", groupID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if _, revertErr := s.store.RemoveADMapping(ctx, groupID, found.DN); revertErr != nil {
			return nil, fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		if _, pruneErr := s.store.PruneUnmappedMemberships(ctx); pruneErr != nil {
			s.logger.Printf("groups: prune after reverting a mapping: %v", pruneErr)
		}
		return nil, auditErr
	}
	return res, nil
}

// UnmapADGroup removes an AD group mapping and forgets the cached memberships
// nothing else justifies. Requires rbac.CanManageGroups. Audited as
// unmapped_ad_group; a failed audit write maps it back.
func (s *Service) UnmapADGroup(ctx context.Context, actor rbac.Actor, groupID, dn string) error {
	if !rbac.CanManageGroups(actor) {
		return rbac.ErrNotPermitted
	}
	g, err := s.store.FindByID(ctx, groupID)
	if err != nil {
		return mapStoreErr(err)
	}
	mappings, err := s.store.ListADMappings(ctx, groupID)
	if err != nil {
		return err
	}
	var existing *db.AppGroupADMapping
	for i := range mappings {
		if strings.EqualFold(mappings[i].DN, dn) {
			existing = &mappings[i]
			break
		}
	}
	if existing == nil {
		return ErrNotFound
	}
	removed, err := s.store.RemoveADMapping(ctx, groupID, existing.DN)
	if err != nil {
		return err
	}
	if !removed {
		return ErrNotFound
	}
	if _, err := s.store.PruneUnmappedMemberships(ctx); err != nil {
		s.logger.Printf("groups: prune memberships after unmapping %q: %v", existing.DN, err)
	}
	detail := map[string]any{"group_name": g.Name, "ad_group_dn": existing.DN, "ad_group_name": existing.Name}
	if err := s.record(ctx, actor, "unmapped_ad_group", groupID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if revertErr := s.store.AddADMapping(ctx, groupID, existing.DN, existing.Name, actorID(actor)); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}

// AddLocalMember adds a local-only account to a group by hand. Requires
// rbac.CanManageGroupLocalMembers (Admin). An AD account is refused: its
// membership comes from AD. Audited as added_group_member; a failed audit write
// removes them again.
func (s *Service) AddLocalMember(ctx context.Context, actor rbac.Actor, groupID, userID string) error {
	if !rbac.CanManageGroupLocalMembers(actor) {
		return rbac.ErrNotPermitted
	}
	g, err := s.store.FindByID(ctx, groupID)
	if err != nil {
		return mapStoreErr(err)
	}
	u, err := s.users.FindByID(ctx, userID)
	if errors.Is(err, db.ErrUserNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("look up user: %w", err)
	}
	if u.LocalUsername == nil {
		return ErrNotLocalAccount
	}
	added, err := s.store.AddLocalMember(ctx, groupID, u.ID, actorID(actor))
	if err != nil {
		return mapStoreErr(err)
	}
	if !added {
		return nil
	}
	detail := map[string]any{"group_name": g.Name, "user_id": u.ID, "user": u.DisplayName}
	if err := s.record(ctx, actor, "added_group_member", groupID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if _, revertErr := s.store.RemoveLocalMember(ctx, groupID, u.ID); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}

// RemoveLocalMember removes a manually added member. Requires
// rbac.CanManageGroupLocalMembers. Audited as removed_group_member; a failed
// audit write adds them back.
func (s *Service) RemoveLocalMember(ctx context.Context, actor rbac.Actor, groupID, userID string) error {
	if !rbac.CanManageGroupLocalMembers(actor) {
		return rbac.ErrNotPermitted
	}
	g, err := s.store.FindByID(ctx, groupID)
	if err != nil {
		return mapStoreErr(err)
	}
	display := userID
	if u, err := s.users.FindByID(ctx, userID); err == nil {
		display = u.DisplayName
	}
	removed, err := s.store.RemoveLocalMember(ctx, groupID, userID)
	if err != nil {
		return err
	}
	if !removed {
		return nil
	}
	detail := map[string]any{"group_name": g.Name, "user_id": userID, "user": display}
	if err := s.record(ctx, actor, "removed_group_member", groupID, detail); err != nil {
		auditErr := fmt.Errorf("record audit: %w", err)
		if _, revertErr := s.store.AddLocalMember(ctx, groupID, userID, actorID(actor)); revertErr != nil {
			return fmt.Errorf("%w (revert also failed: %v)", auditErr, revertErr)
		}
		return auditErr
	}
	return nil
}

// RefreshUser re-reads which mapped AD groups one person is in and caches the
// answer: called after a login and at the hourly session recheck. It never
// returns an error and never blocks anyone: a directory that cannot be reached
// leaves the cached answer as it was (the same "an LDAP blip must not lock
// people out" posture as the access-group recheck), and the failure is logged.
// Derived data, so not audited. dn is the person's cached LDAP DN.
func (s *Service) RefreshUser(ctx context.Context, userID, dn string) {
	if s.directory == nil || dn == "" {
		return
	}
	mapped, err := s.store.MappedDNs(ctx)
	if err != nil {
		s.logger.Printf("groups: list mapped AD groups for user %s: %v", userID, err)
		return
	}
	var in []string
	if len(mapped) > 0 {
		in, err = s.directory.MemberOf(ctx, dn, mapped)
		if err != nil {
			s.logger.Printf("groups: refresh memberships for user %s: %v", userID, err)
			return
		}
	}
	if err := s.store.ReplaceUserMemberships(ctx, userID, in); err != nil {
		s.logger.Printf("groups: cache memberships for user %s: %v", userID, err)
	}
}
