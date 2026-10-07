-- SPDX-License-Identifier: AGPL-3.0-or-later

ALTER TABLE nodes DROP COLUMN comment;
ALTER TABLE nodes DROP COLUMN comment_updated_by;
ALTER TABLE nodes DROP COLUMN comment_updated_at;
