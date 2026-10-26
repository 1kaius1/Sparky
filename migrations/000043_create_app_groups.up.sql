-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Application groups - see SCHEMA.md Application groups and PLANNING.md's
-- 2026-10-26 Decisions Log. A named group of people that one or more AD groups
-- are mapped to; a person is a member through any mapped AD group, or (for a
-- local-only account) because an Admin added them by hand.

CREATE TABLE app_groups (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    created_by uuid REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT app_groups_name_length CHECK (char_length(name) BETWEEN 1 AND 64)
);

-- Names are unique ignoring case ("Data Analytics" and "data analytics" would
-- be indistinguishable in a picker).
CREATE UNIQUE INDEX app_groups_name_lower ON app_groups (lower(name));

-- Which AD groups an application group is mapped to. ad_group_dn is the DN the
-- directory itself returned when the mapping was made (never what a browser
-- posted); ad_group_name is its common name, kept for display. AD compares DNs
-- case-insensitively, so every uniqueness rule and join here uses lower().
CREATE TABLE app_group_ad_groups (
    app_group_id uuid NOT NULL REFERENCES app_groups (id) ON DELETE CASCADE,
    ad_group_dn text NOT NULL,
    ad_group_name text NOT NULL,
    added_by uuid REFERENCES users (id),
    added_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX app_group_ad_groups_unique ON app_group_ad_groups (app_group_id, lower(ad_group_dn));
CREATE INDEX app_group_ad_groups_dn ON app_group_ad_groups (lower(ad_group_dn));

-- Local-only accounts have no AD groups, so an Admin adds them by hand. (Only
-- local accounts: an AD account's membership comes from AD alone - the service
-- enforces that, since it needs the users table.)
CREATE TABLE app_group_local_members (
    app_group_id uuid NOT NULL REFERENCES app_groups (id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    added_by uuid REFERENCES users (id),
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (app_group_id, user_id)
);

-- A cache of which MAPPED AD groups each AD user is in, refreshed at login and
-- at the hourly session recheck. It only ever holds groups some application
-- group is mapped to - never everything a person belongs to - so it stays small
-- and stores no more directory data than the feature needs.
CREATE TABLE user_ad_group_membership (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    ad_group_dn text NOT NULL,
    checked_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX user_ad_group_membership_unique ON user_ad_group_membership (user_id, lower(ad_group_dn));
CREATE INDEX user_ad_group_membership_dn ON user_ad_group_membership (lower(ad_group_dn));

-- Alerts can now be addressed to an application group. Deleting a group removes
-- it from the audiences of the alerts that named it (an alert left with no
-- audience is seen by nobody and ages out with retention).
ALTER TABLE alert_audiences ADD COLUMN app_group_id uuid REFERENCES app_groups (id) ON DELETE CASCADE;

ALTER TABLE alert_audiences DROP CONSTRAINT alert_audiences_shape;
ALTER TABLE alert_audiences ADD CONSTRAINT alert_audiences_shape CHECK (
    (audience_type = 'user' AND user_id IS NOT NULL AND min_tier IS NULL AND app_group_id IS NULL) OR
    (audience_type = 'tier' AND min_tier IS NOT NULL AND user_id IS NULL AND app_group_id IS NULL) OR
    (audience_type = 'group' AND app_group_id IS NOT NULL AND user_id IS NULL AND min_tier IS NULL)
);

CREATE UNIQUE INDEX alert_audiences_group ON alert_audiences (alert_id, app_group_id) WHERE audience_type = 'group';
