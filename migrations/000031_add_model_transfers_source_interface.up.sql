-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Records which of the source node's interfaces a peer-to-peer transfer
-- actually used - full traceability for a slow/failed transfer, same
-- audit-mindedness as this project's other operation logs. NULL means
-- either "internet-sourced (not applicable)" or "peer_node but 'Fastest'
-- auto-selection was used" - both read the same way node_model_inventory
-- and model_profiles' own quantization columns already use NULL/'' for
-- "not applicable"/"default", so this stays consistent rather than
-- inventing a third sentinel.
ALTER TABLE model_transfers ADD COLUMN source_interface text;
