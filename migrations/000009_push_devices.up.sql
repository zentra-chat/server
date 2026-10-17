-- Registered devices for OS level push delivery (FCM / APNs).
--
-- A user can be signed in on several devices at once, and a token can move
-- between accounts when someone logs out on a shared phone, so tokens are
-- unique on their own rather than scoped to a user.
CREATE TABLE IF NOT EXISTS push_devices (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform VARCHAR(16) NOT NULL,
    token TEXT NOT NULL,
    device_name TEXT,
    app_version TEXT,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT push_devices_platform_check CHECK (platform IN ('android', 'ios')),
    CONSTRAINT push_devices_token_unique UNIQUE (token)
);

-- Sending walks every device belonging to a recipient.
CREATE INDEX IF NOT EXISTS idx_push_devices_user_id ON push_devices(user_id);