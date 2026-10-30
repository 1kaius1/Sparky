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

// CanImportModels reports whether actor may scan nodes for model copies
// that are on disk but unknown to Node model inventory, and import them -
// see SCHEMA.md Node model inventory's placed_via. Admin and SuperAdmin
// only, with no permission-override path: unlike a delete, an import
// registers files nobody has vouched for as launchable, and a scan reads
// every node's storage, so it sits at the same infrastructure-level floor
// as CanManageNodes rather than at CanManageModelStore's grantable one.
func CanImportModels(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}

// CanEditNodeComment reports whether actor may set or clear a Node's
// free-text comment field - see SCHEMA.md Nodes' comment column.
// Deliberately broader than CanManageNodes: PowerDev may annotate which
// team/model is using a node without being able to edit its
// infrastructure settings (runtime_backend, default_transfer_interface,
// SSH identity) - same tier-native shape as CanManageProfiles, since this
// is informational annotation, not infrastructure configuration.
func CanEditNodeComment(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	switch actor.Tier {
	case db.TierPowerDev, db.TierAdmin:
		return true
	default:
		return false
	}
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

// CanManageStaleContainers reports whether actor may configure stale container
// monitoring and use the manual cleanup page, which removes containers from a
// node. Admin/SuperAdmin only (PLANNING.md, 2026-10-26). A distinct function
// rather than CanViewSettings or CanManageNodes, even though all share the
// same tier floor today: removing a node's containers is a different
// capability from reading settings, and could be granted differently later.
func CanManageStaleContainers(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}

// CanManageEmail reports whether actor may configure email delivery (the SMTP
// relay, the digest time) and send the test email. Admin/SuperAdmin only
// (PLANNING.md, 2026-10-26). A distinct function rather than CanViewSettings,
// even though both share a tier floor today: email settings decide where
// alert text is sent, and could be granted differently later.
func CanManageEmail(actor Actor) bool {
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

// CanCreateLocalAccount reports whether actor may create a new local-only
// account - see SCHEMA.md Users' Local-only accounts subsection. Same
// unconditional-by-tier shape as CanManageNodes: no permission-override
// path, Admin/SuperAdmin only.
func CanCreateLocalAccount(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}

// CanResetLocalAccountPassword reports whether actor may reset another
// local-only account's password - see SCHEMA.md Users' Local-only accounts
// subsection. Same unconditional-by-tier shape as CanCreateLocalAccount; a
// distinct function even though the two share a tier floor today, matching
// this file's existing precedent (CanViewSettings vs. CanViewAuditLog/
// CanViewUsers) of not conflating capabilities that could diverge later.
func CanResetLocalAccountPassword(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}

// CanSetUserEmail reports whether actor may set or clear another local
// account's email address, and see every account's address on the Users page.
// Same unconditional-by-tier shape as CanResetLocalAccountPassword and for the
// same reason a distinct function: an address decides where a person's alert
// text is sent, so it is its own capability even though it shares a floor.
func CanSetUserEmail(actor Actor) bool {
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

// CanSetHostContainerOptions reports whether actor may set the host-level
// part of a Model profile's container options - extra bind mounts, device
// passthrough, added capabilities, privileged mode, ulimits, security options
// and the host PID namespace (launchopts.HostLevel). Each of those reaches
// into the node, and a profile launches on a node as the agent's account, so
// they are Admin and SuperAdmin only, with no permission-override path - the
// same infrastructure-level floor as CanManageNodes, not the profile-editing
// floor of CanManageProfiles. A PowerDev can still edit a profile an Admin
// gave such options; they stay as they were (profiles.Service).
func CanSetHostContainerOptions(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}

// CanViewInstanceLogs reports whether actor may read a container's output -
// an archived log, and later the live tail. The floor is Developer, the same
// people who can launch an instance, because they are the ones who need to
// see why theirs failed. Logs can contain prompt or request content and
// tokens, so this is deliberately a wider exposure than the audit log's
// Admin floor (CanViewAuditLog) - a maintainer decision recorded in
// PLANNING.md's 2026-10-08 Decisions Log - and no permission override widens
// or narrows it. A Read-only user cannot see logs.
func CanViewInstanceLogs(actor Actor) bool {
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

// CanResolveAnyAlert reports whether actor may close an alert for everyone
// who can see it, whatever its audience - see SCHEMA.md Alerts. Admin and
// SuperAdmin only. A person an alert is addressed to alone may also resolve
// it; that case depends on the alert's audience, so internal/alerts decides
// it, not this pure tier check. Dismissing (hiding an alert for oneself) has
// no floor at all.
func CanResolveAnyAlert(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}

// CanManageGroups reports whether actor may see the Groups page and create,
// rename and delete application groups and map AD groups to them - see
// SCHEMA.md Application groups. PowerDev and above, as the maintainer decided
// (PLANNING.md, 2026-10-26). No permission-override path: like the other
// management floors it is by tier.
func CanManageGroups(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	switch actor.Tier {
	case db.TierPowerDev, db.TierAdmin:
		return true
	default:
		return false
	}
}

// CanManageGroupLocalMembers reports whether actor may add or remove a local
// account as a manual member of an application group. Admin and SuperAdmin
// only - narrower than CanManageGroups, because a manual membership is the one
// grant that no AD group backs and groups will later gate shared capacity. The
// maintainer's decision, 2026-10-26.
func CanManageGroupLocalMembers(actor Actor) bool {
	if actor.IsSuperAdmin {
		return true
	}
	return actor.Tier == db.TierAdmin
}
