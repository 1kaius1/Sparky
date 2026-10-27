-- SPDX-License-Identifier: AGPL-3.0-or-later

-- User email addresses - see SCHEMA.md Users (email) and Email confirmations
-- (PLANNING.md Decisions Log, 2026-10-26).
--
-- users.email is the address alert email is sent to; NULL means none. Where it
-- came from is email_source, and the two are NULL together:
--   directory - read from AD's mail attribute at every login. Only an
--               AD-backed account can have it, and it is never edited in
--               Sparky: a person's address changes in the directory.
--   self      - a local-only account's own address, used only once its owner
--               confirmed it by opening a link sent to it.
--   admin     - a local-only account's address set by an Admin, with or
--               without the confirmation step (the Admin chooses).
-- Several accounts may share an address (a team mailbox), so it is not unique.
CREATE TYPE email_source AS ENUM ('directory', 'self', 'admin');

ALTER TABLE users
    ADD COLUMN email text,
    ADD COLUMN email_source email_source,
    ADD CONSTRAINT users_email_with_source CHECK ((email IS NULL) = (email_source IS NULL)),
    ADD CONSTRAINT users_email_source_matches_identity CHECK (
        email_source IS NULL
        OR (ad_sid IS NOT NULL AND email_source = 'directory')
        OR (ad_sid IS NULL AND email_source IN ('self', 'admin'))
    );

-- A change of address waiting for its owner to confirm it. One per user: a new
-- request replaces the old. Only the SHA-256 of the token is stored, so a copy
-- of the database cannot confirm anything; the token itself is in the email
-- sent to the new address. source and requested_by say who asked: the owner
-- (self) or an Admin (admin; requested_by is that Admin, NULL for the
-- break-glass SuperAdmin).
CREATE TABLE email_confirmations (
    user_id uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    email text NOT NULL,
    token_hash text NOT NULL,
    source email_source NOT NULL,
    requested_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CONSTRAINT email_confirmations_source CHECK (source IN ('self', 'admin'))
);

CREATE UNIQUE INDEX email_confirmations_token_hash ON email_confirmations (token_hash);
