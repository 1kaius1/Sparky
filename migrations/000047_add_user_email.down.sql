-- SPDX-License-Identifier: AGPL-3.0-or-later

DROP TABLE email_confirmations;

ALTER TABLE users
    DROP CONSTRAINT users_email_source_matches_identity,
    DROP CONSTRAINT users_email_with_source,
    DROP COLUMN email_source,
    DROP COLUMN email;

DROP TYPE email_source;
