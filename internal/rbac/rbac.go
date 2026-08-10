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
