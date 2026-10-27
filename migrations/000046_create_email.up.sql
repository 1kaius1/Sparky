-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Email delivery - see SCHEMA.md Email settings and Email queue (PLANNING.md
-- Decisions Log, 2026-10-26). Sparky sends only through an SMTP relay.
--
-- email_settings is a singleton, seeded here with email off so the sender
-- always has a setting to read. The relay's password is deliberately NOT here:
-- it comes from the SMTP_PASSWORD environment variable, like every other
-- secret. The CHECK refuses to turn email on without a relay and a from
-- address, so "enabled" can never mean "enabled but unusable".
CREATE TYPE email_tls_mode AS ENUM ('none', 'starttls', 'tls');

CREATE TABLE email_settings (
    id boolean PRIMARY KEY DEFAULT true,
    enabled boolean NOT NULL DEFAULT false,
    host text NOT NULL DEFAULT '',
    port integer NOT NULL DEFAULT 587,
    tls_mode email_tls_mode NOT NULL DEFAULT 'starttls',
    username text NOT NULL DEFAULT '',
    from_address text NOT NULL DEFAULT '',
    from_name text NOT NULL DEFAULT 'Sparky',
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT email_settings_singleton CHECK (id),
    CONSTRAINT email_settings_port_range CHECK (port >= 1 AND port <= 65535),
    CONSTRAINT email_settings_enabled_needs_relay CHECK (NOT enabled OR (host <> '' AND from_address <> ''))
);

INSERT INTO email_settings (id) VALUES (true);

-- email_queue holds mail waiting to go and a record of what went. A row is
-- written when something wants to send (an alert, a digest, a confirmation, a
-- test) and a worker delivers it, so a mail outage never blocks the
-- application. It is a record, not a mailbox: the subject and body are what
-- was sent (summaries and links only, never log or prompt content).
--
-- attempts counts tries; next_attempt_at is when the worker may try again,
-- and also the lease on a row it has claimed, so a crash mid-send retries
-- later instead of losing the mail. status moves pending -> sent, or pending ->
-- failed when the relay refuses the message, the attempts run out, or it is
-- too old to be worth sending. last_error says why, for the Settings page.
-- user_id is informational and set NULL if the user is deleted: the record
-- of what was sent outlives the account.
CREATE TYPE email_kind AS ENUM ('alert', 'digest', 'confirmation', 'test');
CREATE TYPE email_status AS ENUM ('pending', 'sent', 'failed');

CREATE TABLE email_queue (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind email_kind NOT NULL,
    to_address text NOT NULL,
    user_id uuid REFERENCES users (id) ON DELETE SET NULL,
    subject text NOT NULL,
    body text NOT NULL,
    status email_status NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz,
    CONSTRAINT email_queue_sent_consistent CHECK ((status = 'sent') = (sent_at IS NOT NULL)),
    CONSTRAINT email_queue_attempts_nonnegative CHECK (attempts >= 0)
);

CREATE INDEX email_queue_due ON email_queue (next_attempt_at) WHERE status = 'pending';
CREATE INDEX email_queue_created_at ON email_queue (created_at);
