-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Group audiences cannot exist without app_groups, so they go first. The
-- 'group' enum value itself stays (see migration 000042's down step).
DELETE FROM alert_audiences WHERE audience_type = 'group';

DROP INDEX alert_audiences_group;

ALTER TABLE alert_audiences DROP CONSTRAINT alert_audiences_shape;
ALTER TABLE alert_audiences ADD CONSTRAINT alert_audiences_shape CHECK (
    (audience_type = 'user' AND user_id IS NOT NULL AND min_tier IS NULL) OR
    (audience_type = 'tier' AND min_tier IS NOT NULL AND user_id IS NULL)
);
ALTER TABLE alert_audiences DROP COLUMN app_group_id;

DROP TABLE user_ad_group_membership;
DROP TABLE app_group_local_members;
DROP TABLE app_group_ad_groups;
DROP TABLE app_groups;
