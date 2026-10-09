-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Postgres cannot drop a single enum value, so the type is replaced. Rows
-- holding the value are first marked unhealthy - the closest value that
-- existed before this migration, and the one an unanswered probe would have
-- produced.
UPDATE running_instances SET health_status = 'unhealthy' WHERE health_status = 'dead';

ALTER TABLE running_instances ALTER COLUMN health_status DROP DEFAULT;
ALTER TYPE instance_health_status RENAME TO instance_health_status_old;
CREATE TYPE instance_health_status AS ENUM ('healthy', 'unhealthy', 'unknown');
ALTER TABLE running_instances ALTER COLUMN health_status TYPE instance_health_status USING health_status::text::instance_health_status;
ALTER TABLE running_instances ALTER COLUMN health_status SET DEFAULT 'unknown';
DROP TYPE instance_health_status_old;
