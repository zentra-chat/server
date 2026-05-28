-- Migration: 000004_plugin_system_v2
-- Description: Add plugin_state table for per-plugin KV storage, WASM object key columns

-- Plugin KV state store (used by WASM runtime host functions).
-- channel_id uses a sentinel zero-UUID for global (non-channel-scoped) keys,
-- keeping the PK simple and index-friendly.
CREATE TABLE IF NOT EXISTS plugin_state (
    community_id UUID NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    plugin_id UUID NOT NULL REFERENCES plugins(id) ON DELETE CASCADE,
    key VARCHAR(512) NOT NULL,
    value BYTEA NOT NULL,
    channel_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (community_id, plugin_id, key, channel_id)
);

CREATE INDEX IF NOT EXISTS idx_plugin_state_lookup
    ON plugin_state(community_id, plugin_id);

CREATE INDEX IF NOT EXISTS idx_plugin_state_channel
    ON plugin_state(community_id, plugin_id, channel_id);

-- WASM binary and UI bundle object keys on the plugins table
ALTER TABLE plugins
    ADD COLUMN IF NOT EXISTS wasm_object_key TEXT,
    ADD COLUMN IF NOT EXISTS ui_bundle_object_key TEXT;

-- updated_at trigger for plugin_state
DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'update_plugin_state_updated_at') THEN
    CREATE TRIGGER update_plugin_state_updated_at
        BEFORE UPDATE ON plugin_state
        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
END IF; END $$;
