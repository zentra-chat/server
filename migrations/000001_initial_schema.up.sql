-- Migration: 000001_initial_schema
-- Description: Create initial database schema for Zentra

-- Users table
CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    username VARCHAR(32) NOT NULL UNIQUE,
    email VARCHAR(255) NOT NULL UNIQUE,
    password_hash VARCHAR(255) NOT NULL,
    display_name VARCHAR(64),
    avatar_url TEXT,
    bio TEXT,
    status user_status DEFAULT 'offline',
    custom_status VARCHAR(128),
    email_verified BOOLEAN DEFAULT FALSE,
    two_factor_enabled BOOLEAN DEFAULT FALSE,
    two_factor_secret VARCHAR(32),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_users_username ON users(username);
CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
CREATE INDEX IF NOT EXISTS idx_users_username_trgm ON users USING gin(username gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_users_display_name_trgm ON users USING gin(display_name gin_trgm_ops);

-- User sessions for refresh tokens
CREATE TABLE IF NOT EXISTS user_sessions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    refresh_token_hash VARCHAR(255) NOT NULL,
    device_info TEXT,
    ip_address INET,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    revoked_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_user_sessions_user_id ON user_sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_user_sessions_expires_at ON user_sessions(expires_at);

-- Communities (like Discord servers)
CREATE TABLE IF NOT EXISTS communities (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name VARCHAR(100) NOT NULL,
    description TEXT,
    icon_url TEXT,
    banner_url TEXT,
    owner_id UUID NOT NULL REFERENCES users(id),
    is_public BOOLEAN DEFAULT FALSE,
    is_open BOOLEAN DEFAULT FALSE,
    member_count INTEGER DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_communities_owner_id ON communities(owner_id);
CREATE INDEX IF NOT EXISTS idx_communities_is_public ON communities(is_public) WHERE is_public = TRUE;
CREATE INDEX IF NOT EXISTS idx_communities_name_trgm ON communities USING gin(name gin_trgm_ops);

-- Community members
CREATE TABLE IF NOT EXISTS community_members (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    community_id UUID NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    nickname VARCHAR(64),
    joined_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(community_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_community_members_community_id ON community_members(community_id);
CREATE INDEX IF NOT EXISTS idx_community_members_user_id ON community_members(user_id);

-- Community invites
CREATE TABLE IF NOT EXISTS community_invites (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    community_id UUID NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    code VARCHAR(16) NOT NULL UNIQUE,
    created_by UUID NOT NULL REFERENCES users(id),
    max_uses INTEGER,
    use_count INTEGER DEFAULT 0,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_community_invites_code ON community_invites(code);
CREATE INDEX IF NOT EXISTS idx_community_invites_community_id ON community_invites(community_id);

-- Roles for fine-grained permissions
CREATE TABLE IF NOT EXISTS roles (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    community_id UUID NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    name VARCHAR(64) NOT NULL,
    color VARCHAR(7),
    position INTEGER DEFAULT 0,
    permissions BIGINT DEFAULT 0,
    is_default BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_roles_community_id ON roles(community_id);

-- Member roles junction table
CREATE TABLE IF NOT EXISTS member_roles (
    member_id UUID NOT NULL REFERENCES community_members(id) ON DELETE CASCADE,
    role_id UUID NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    PRIMARY KEY (member_id, role_id)
);

-- Channel categories
CREATE TABLE IF NOT EXISTS channel_categories (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    community_id UUID NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    name VARCHAR(64) NOT NULL,
    position INTEGER DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_channel_categories_community_id ON channel_categories(community_id);

-- Channels
CREATE TABLE IF NOT EXISTS channels (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    community_id UUID NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    category_id UUID REFERENCES channel_categories(id) ON DELETE SET NULL,
    name VARCHAR(64) NOT NULL,
    topic TEXT,
    type channel_type DEFAULT 'text',
    position INTEGER DEFAULT 0,
    is_nsfw BOOLEAN DEFAULT FALSE,
    slowmode_seconds INTEGER DEFAULT 0,
    last_message_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_channels_community_id ON channels(community_id);
CREATE INDEX IF NOT EXISTS idx_channels_category_id ON channels(category_id);

-- Channel permission overwrites
CREATE TABLE IF NOT EXISTS channel_permissions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    channel_id UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    target_type VARCHAR(10) NOT NULL, -- 'role' or 'member'
    target_id UUID NOT NULL,
    allow_permissions BIGINT DEFAULT 0,
    deny_permissions BIGINT DEFAULT 0,
    UNIQUE(channel_id, target_type, target_id)
);

CREATE INDEX IF NOT EXISTS idx_channel_permissions_channel_id ON channel_permissions(channel_id);

-- Messages
CREATE TABLE IF NOT EXISTS messages (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    channel_id UUID NOT NULL,
    author_id UUID NOT NULL,
    content TEXT,
    encrypted_content BYTEA,
    reply_to_id UUID,
    is_edited BOOLEAN DEFAULT FALSE,
    is_pinned BOOLEAN DEFAULT FALSE,
    reactions JSONB DEFAULT '{}',
    link_previews JSONB DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_messages_channel_id ON messages(channel_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_author_id ON messages(author_id);
CREATE INDEX IF NOT EXISTS idx_messages_reply_to_id ON messages(reply_to_id);

-- Message attachments
CREATE TABLE IF NOT EXISTS message_attachments (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    message_id UUID,
    dm_message_id UUID,
    dm_conversation_id UUID,
    uploader_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    filename VARCHAR(255) NOT NULL,
    file_url TEXT NOT NULL,
    file_size BIGINT NOT NULL,
    content_type VARCHAR(128),
    thumbnail_url TEXT,
    width INTEGER,
    height INTEGER,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_message_attachments_message_id ON message_attachments(message_id);
CREATE INDEX IF NOT EXISTS idx_message_attachments_dm_message_id ON message_attachments(dm_message_id);
CREATE INDEX IF NOT EXISTS idx_message_attachments_dm_conversation_id ON message_attachments(dm_conversation_id);

-- Direct message conversations
CREATE TABLE IF NOT EXISTS dm_conversations (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Direct message participants
CREATE TABLE IF NOT EXISTS dm_participants (
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    last_read_at TIMESTAMPTZ,
    PRIMARY KEY (conversation_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_dm_participants_user_id ON dm_participants(user_id);

-- Direct messages (encrypted at rest)
CREATE TABLE IF NOT EXISTS direct_messages (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    sender_id UUID NOT NULL REFERENCES users(id),
    encrypted_content BYTEA NOT NULL,
    nonce BYTEA NOT NULL,
    reply_to_id UUID,
    is_edited BOOLEAN DEFAULT FALSE,
    reactions JSONB DEFAULT '{}'::jsonb,
    link_previews JSONB DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_direct_messages_conversation_id ON direct_messages(conversation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_direct_messages_sender_id ON direct_messages(sender_id);
CREATE INDEX IF NOT EXISTS idx_direct_messages_reply_to_id ON direct_messages(reply_to_id);

-- User blocks
CREATE TABLE IF NOT EXISTS user_blocks (
    blocker_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    blocked_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (blocker_id, blocked_id)
);

-- User settings
CREATE TABLE IF NOT EXISTS user_settings (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    theme VARCHAR(20) DEFAULT 'dark',
    notifications_enabled BOOLEAN DEFAULT TRUE,
    sound_enabled BOOLEAN DEFAULT TRUE,
    compact_mode BOOLEAN DEFAULT FALSE,
    settings_json JSONB DEFAULT '{}',
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Audit log for moderation
CREATE TABLE IF NOT EXISTS audit_logs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    community_id UUID REFERENCES communities(id) ON DELETE CASCADE,
    actor_id UUID NOT NULL REFERENCES users(id),
    action VARCHAR(64) NOT NULL,
    target_type VARCHAR(32),
    target_id UUID,
    details JSONB,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_audit_logs_community_id ON audit_logs(community_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_actor_id ON audit_logs(actor_id);

-- Function to update updated_at timestamp
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ language 'plpgsql';

-- Apply update triggers
DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'update_users_updated_at') THEN
    CREATE TRIGGER update_users_updated_at BEFORE UPDATE ON users
        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
END IF; END $$;

DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'update_communities_updated_at') THEN
    CREATE TRIGGER update_communities_updated_at BEFORE UPDATE ON communities
        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
END IF; END $$;

DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'update_channels_updated_at') THEN
    CREATE TRIGGER update_channels_updated_at BEFORE UPDATE ON channels
        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
END IF; END $$;

DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'update_roles_updated_at') THEN
    CREATE TRIGGER update_roles_updated_at BEFORE UPDATE ON roles
        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
END IF; END $$;

DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'update_dm_conversations_updated_at') THEN
    CREATE TRIGGER update_dm_conversations_updated_at BEFORE UPDATE ON dm_conversations
        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
END IF; END $$;

DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'update_user_settings_updated_at') THEN
    CREATE TRIGGER update_user_settings_updated_at BEFORE UPDATE ON user_settings
        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
END IF; END $$;

-- Function to update community member count
CREATE OR REPLACE FUNCTION update_community_member_count()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        UPDATE communities SET member_count = member_count + 1 WHERE id = NEW.community_id;
    ELSIF TG_OP = 'DELETE' THEN
        UPDATE communities SET member_count = member_count - 1 WHERE id = OLD.community_id;
    END IF;
    RETURN NULL;
END;
$$ language 'plpgsql';

DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'update_member_count_trigger') THEN
    CREATE TRIGGER update_member_count_trigger
        AFTER INSERT OR DELETE ON community_members
        FOR EACH ROW EXECUTE FUNCTION update_community_member_count();
END IF; END $$;
