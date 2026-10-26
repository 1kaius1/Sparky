-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Deliberately does nothing. PostgreSQL cannot remove a value from an enum
-- type, so the 'group' value stays in alert_audience_type after a rollback;
-- unused, it is harmless. Migration 000043's down step removes every row that
-- uses it.
SELECT 1;
