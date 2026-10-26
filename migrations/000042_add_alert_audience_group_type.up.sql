-- SPDX-License-Identifier: AGPL-3.0-or-later

-- An alert can be addressed to an application group (PLANNING.md, 2026-10-26).
--
-- This is its own migration on purpose: PostgreSQL cannot use a new enum
-- value in the same transaction that adds it, and the next migration's CHECK
-- constraint uses 'group'. golang-migrate runs each file as one unit, so the
-- value has to be committed by a migration of its own first.
ALTER TYPE alert_audience_type ADD VALUE IF NOT EXISTS 'group';
