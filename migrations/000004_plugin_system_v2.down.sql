-- Migration: 000004_plugin_system_v2 (down)
-- Description: Remove plugin KV state and WASM columns

DROP TABLE IF EXISTS plugin_state;

ALTER TABLE plugins
    DROP COLUMN IF EXISTS wasm_object_key,
    DROP COLUMN IF EXISTS ui_bundle_object_key;
