-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Engine transfers - see SCHEMA.md Engine transfers. Operation log for a
-- compiled-engine binary provisioning run onto a node, the same shape as
-- Model transfers but for engine software (llama.cpp today) rather than
-- model weights - see PLANNING.md's 2026-08-15 Decisions Log entry for why
-- this is a separate table rather than folding into model_transfers: the
-- source (Sparky's own maintainer-built GitHub Releases, checksum-verified)
-- and destination shape (a versioned install directory, not a single
-- downloaded file tree) are different enough to warrant it, and conflating
-- "model weights present on this node" with "engine binaries present on
-- this node" would make node_model_inventory's own meaning less precise.
--
-- engine_type reuses model_profiles' own profile_engine_type enum rather
-- than minting a new one, keeping the vocabulary consistent even though
-- only 'llamacpp' rows are realistic through this path today - Python-based
-- engines (vllm, aphrodite) get a different v0.3.0 clone-and-pip-install
-- mechanism, not this one.
--
-- requested_by is nullable for the same reason model_transfers.requested_by
-- is: the break-glass SuperAdmin can initiate this too and is not a Users
-- row - see SCHEMA.md Break-glass credential.

CREATE TYPE engine_transfer_status AS ENUM ('queued', 'transferring', 'completed', 'failed', 'cancelled');

CREATE TABLE engine_transfers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    dest_node_id uuid NOT NULL REFERENCES nodes (id),
    engine_type profile_engine_type NOT NULL,
    version text NOT NULL,
    status engine_transfer_status NOT NULL DEFAULT 'queued',
    bytes_transferred bigint NOT NULL DEFAULT 0,
    bytes_total bigint NOT NULL DEFAULT 0,
    requested_by uuid REFERENCES users (id),
    requested_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    error_message text
);
