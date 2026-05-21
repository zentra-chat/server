-- Migration: 000013_channel_read_state (down)
-- Description: Remove channel read state tracking

DROP INDEX IF EXISTS idx_channel_read_states_channel;
DROP INDEX IF EXISTS idx_channel_read_states_user;
DROP TABLE IF EXISTS channel_read_states;
