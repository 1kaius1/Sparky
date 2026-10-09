-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Adds 'dead' to instance_health_status: the instance's container or process
-- is not running although the central app expects it to be (exited,
-- OOM-killed, or removed behind Sparky's back). Without it such an instance
-- stayed 'running' indefinitely with a stale health value, and its profile
-- could not be launched again. The row's lifecycle status deliberately stays
-- 'running' - the operator sees the instance as dead and Unload clears it.
--
-- A new enum value cannot be used in the transaction that adds it, and
-- nothing here does.
ALTER TYPE instance_health_status ADD VALUE IF NOT EXISTS 'dead';
