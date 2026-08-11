-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Node bearer token - see SCHEMA.md Nodes and ARCHITECTURE.md Protocol
-- ("Bearer token presented by the agent at connect time"). Same storage
-- pattern as break_glass_credential.password_hash: only the hash is ever
-- persisted, never the plaintext - see internal/auth's node token helpers
-- for why this is a fast SHA-256 hash rather than Argon2id, unlike that
-- password hash.
ALTER TABLE nodes ADD COLUMN bearer_token_hash text NOT NULL;
