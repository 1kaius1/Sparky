// SPDX-License-Identifier: AGPL-3.0-or-later

// Package rbac resolves a session's tier, evaluates elevation rules, and
// checks permission overrides - see ARCHITECTURE.md RBAC & Permission
// Overrides. It never accesses the database directly (see CLAUDE.md); the
// decision functions here are pure, and Service in service.go is the thin
// orchestration layer that persists a permitted decision via
// internal/db.
package rbac

import "github.com/1kaius1/Sparky/internal/db"

// Actor is whoever is attempting an RBAC-gated action. The SuperAdmin is
// not a Users row (see SCHEMA.md Break-glass credential), so it cannot be
// represented as a *db.User - IsSuperAdmin is the distinct signal for it.
type Actor struct {
	IsSuperAdmin bool
	UserID       string  // the actor's own Users.id; empty when IsSuperAdmin
	Tier         db.Tier // meaningless when IsSuperAdmin
}

// adminRange is the set of tiers an Admin actor has any authority over -
// see SCHEMA.md Users, Elevation rules. Admin tier itself, and the
// SuperAdmin identity, are both entirely outside an Admin actor's
// authority: an Admin can neither promote a user to Admin nor change the
// tier of an existing Admin.
var adminRange = []db.Tier{db.TierReadOnly, db.TierDeveloper, db.TierPowerDev}

func rangeIndex(tier db.Tier) int {
	for i, t := range adminRange {
		if t == tier {
			return i
		}
	}
	return -1
}

// CanElevate reports whether actor may change a user's tier from fromTier
// to toTier - see SCHEMA.md Users, Elevation rules.
//
// SuperAdmin can set any user to any tier, always. An Admin actor may only
// move a tier by exactly one step, up or down, within
// {read_only, developer, power_dev} - never touching the Admin tier itself
// as either the source or the destination. No other tier may elevate
// anyone.
func CanElevate(actor Actor, fromTier, toTier db.Tier) bool {
	if actor.IsSuperAdmin {
		return true
	}
	if actor.Tier != db.TierAdmin {
		return false
	}

	fromIdx := rangeIndex(fromTier)
	toIdx := rangeIndex(toTier)
	if fromIdx == -1 || toIdx == -1 {
		return false
	}

	step := toIdx - fromIdx
	return step == 1 || step == -1
}

// CanManageNodes reports whether actor may register or edit Nodes - see
// CLAUDE.md Frontend Conventions, Nodes' sidebar tier ("Admin edit"). No
// permission-override path exists for this one, unlike
// CanManageModelStore: node registration is infrastructure-level, not a
// per-user grantable exception.
func CanManageNodes(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}

// CanViewAuditLog reports whether actor may view the Audit log - see
// CLAUDE.md Frontend Conventions, Audit log's sidebar tier ("Admin").
// Unlike Dashboard/Nodes/Model profiles/Transfers, the Audit log's floor
// is Admin, not Read-only - no permission-override path exists for this
// one either, same unconditional-by-tier shape as CanManageNodes.
func CanViewAuditLog(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}

// CanViewUsers reports whether actor may view the Users & permissions
// roster - see CLAUDE.md Frontend Conventions, Users & permissions'
// sidebar tier ("Admin"). Same unconditional-by-tier shape as
// CanViewAuditLog: no permission-override path, Admin/SuperAdmin only.
func CanViewUsers(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}

// CanViewSettings reports whether actor may view the Settings page (the
// Metrics export config and Audit settings singleton rows) - see
// CLAUDE.md Frontend Conventions, Settings' sidebar tier ("Admin"). Same
// unconditional-by-tier shape as CanViewAuditLog/CanViewUsers: no
// permission-override path, Admin/SuperAdmin only. A distinct function
// rather than reusing either of those, even though all three share the
// same tier floor today - Settings, Audit log, and the Users roster are
// three different capabilities that could diverge in who may view them
// later.
func CanViewSettings(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}

// CanManageModelStore reports whether actor has the manage_model_store
// capability (download and delete models) - see SCHEMA.md Permission
// overrides. Admin and SuperAdmin always have it implicitly. PowerDev has
// it only when hasOverride reflects an existing grant row. No other tier
// has it, regardless of hasOverride - the override table is only ever
// meaningful for PowerDev-tier users.
func CanManageModelStore(actor Actor, hasOverride bool) bool {
	if actor.IsSuperAdmin {
		return true
	}
	if actor.Tier == db.TierAdmin {
		return true
	}
	return actor.Tier == db.TierPowerDev && hasOverride
}

// CanManageProfiles reports whether actor may create, edit, or delete
// Model profiles - see CLAUDE.md Frontend Conventions, Model profiles'
// sidebar tier ("PowerDev create"). Unlike CanManageModelStore, this is
// tier-native for PowerDev - no permission-override path, same
// unconditional-by-tier shape as CanManageNodes.
func CanManageProfiles(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	if actor.Tier == db.TierAdmin {
		return true
	}
	return actor.Tier == db.TierPowerDev
}

// CanLaunchInstances reports whether actor may load or unload a Running
// instance - see CLAUDE.md Frontend Conventions, Model profiles' sidebar
// tier ("Developer launch"). Deliberately a lower bar than
// CanManageProfiles: a Developer may launch a profile someone else
// (PowerDev+) created, but may not create, edit, or delete the profile
// itself. One function guards both load and unload, same reasoning as
// CanManageModelStore guarding both download and delete - see SCHEMA.md
// Permission overrides.
func CanLaunchInstances(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	switch actor.Tier {
	case db.TierDeveloper, db.TierPowerDev, db.TierAdmin:
		return true
	default:
		return false
	}
}
