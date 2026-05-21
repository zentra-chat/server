-- Migration: 000013_channel_read_state
-- Description: Track per-user read state for channels

CREATE TABLE channel_read_states (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    last_read_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, channel_id)
);

CREATE INDEX IF NOT EXISTS idx_channel_read_states_user ON channel_read_states(user_id);
CREATE INDEX IF NOT EXISTS idx_channel_read_states_channel ON channel_read_states(channel_id);
