-- SPDX-License-Identifier: AGPL-3.0-or-later

-- A model imported from disk (an Admin adopting a copy that was placed on a
-- node outside Sparky) was never placed by a transfer, so there is no
-- model_transfers row for placed_via to reference. NULL means exactly that:
-- see SCHEMA.md Node model inventory. Every existing row keeps its value.

ALTER TABLE node_model_inventory ALTER COLUMN placed_via DROP NOT NULL;
