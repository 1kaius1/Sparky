-- SPDX-License-Identifier: AGPL-3.0-or-later

ALTER TABLE nodes DROP COLUMN ssh_public_key;
ALTER TABLE nodes DROP COLUMN ssh_host_public_key;
ALTER TABLE nodes DROP COLUMN default_transfer_interface;
