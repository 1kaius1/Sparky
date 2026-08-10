-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Audit log - see SCHEMA.md Audit log. Append-only record of every
-- state-changing action, including the SuperAdmin's - actor_id is
-- nullable specifically to represent the break-glass account, which is
-- not a Users row (see SCHEMA.md Break-glass credential);
-- is_superadmin_action distinguishes that case from an actor_id that is
-- merely missing.

CREATE TABLE audit_log (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id uuid REFERENCES users (id),
    is_superadmin_action boolean NOT NULL DEFAULT false,
    action text NOT NULL,
    object_type text NOT NULL,
    object_id uuid NOT NULL,
    detail jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Supports both the default chronological view and a per-user filter on
-- the future Audit log page (ARCHITECTURE.md HTTP/API + Dashboard).
CREATE INDEX audit_log_created_at_idx ON audit_log (created_at);
CREATE INDEX audit_log_actor_id_idx ON audit_log (actor_id);
