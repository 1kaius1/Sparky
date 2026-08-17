-- SPDX-License-Identifier: AGPL-3.0-or-later

-- GGUF quantization selection, download side - see SCHEMA.md Model
-- transfers and PLANNING.md's Decisions Log. NULL/empty means "download
-- the whole repo" (today's behavior, unchanged); a non-empty value
-- restricts agent/transfer.Executor.Download to just the one matching
-- .gguf file.

ALTER TABLE model_transfers ADD COLUMN quantization text;
