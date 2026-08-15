-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Node engine inventory - see SCHEMA.md Node engine inventory. Current-state
-- answer to "which engine binary versions are installed on this node right
-- now", the same shape as Node model inventory but for engine software.
--
-- Unlike node_model_inventory's (node_id, model_ref) primary key, this
-- table's primary key also includes version: multiple versions of the same
-- engine are expected to coexist side by side on a node by design (not a
-- transient state) - see PLANNING.md's 2026-08-15 Decisions Log entry and
-- the deferred per-profile engine_version pinning follow-up item this is
-- built to support. A newer provisioning run never deletes an older
-- version's row; cleanup is a separate, later concern.
--
-- install_path records the node-local absolute path the agent actually
-- installed this version into (SPARKY_ENGINE_INSTALL_PATH/<engine_type>/
-- <version>), so a later "resolve a pinned version to a binary path" step
-- can read it directly rather than reconstructing the convention.

CREATE TABLE node_engine_inventory (
    node_id uuid NOT NULL REFERENCES nodes (id),
    engine_type profile_engine_type NOT NULL,
    version text NOT NULL,
    status inventory_status NOT NULL,
    install_path text NOT NULL,
    size_bytes bigint NOT NULL,
    placed_at timestamptz NOT NULL DEFAULT now(),
    placed_via uuid NOT NULL REFERENCES engine_transfers (id),
    PRIMARY KEY (node_id, engine_type, version)
);
