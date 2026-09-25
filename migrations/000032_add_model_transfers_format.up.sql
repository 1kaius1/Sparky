-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Records which format (safetensors / gguf) a peer_node transfer is
-- moving. A peer transfer copies an existing inventory entry, whose
-- (model_ref, quantization, format) key is exact - so the destination's
-- new inventory entry must reuse the source's real format rather than the
-- quantization-presence guess an internet download still relies on
-- (internal/transfers), which would mislabel, e.g., a safetensors entry
-- with quantization 'FP16' as gguf. NULL means "not recorded" - every
-- internet-sourced transfer, whose format is still inferred until
-- agent/modelinspect determines it from the downloaded file.
ALTER TABLE model_transfers ADD COLUMN format model_format;
