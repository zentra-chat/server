-- Migration: 000014_messages_unpartition
-- Description: Migrate messages from RANGE(created_at) partitioning to a plain table
-- Only runs on deployments with the old RANGE partition schema

DO $$
DECLARE
    is_old_schema BOOLEAN;
BEGIN
    SELECT EXISTS (
        SELECT 1 FROM pg_partitioned_table pt
        JOIN pg_class c ON c.oid = pt.partrelid
        WHERE c.relname = 'messages' AND pt.partstrat = 'r'
    ) INTO is_old_schema;

    IF is_old_schema THEN
        CREATE TABLE messages_v2 (
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

        INSERT INTO messages_v2 SELECT * FROM messages;

        DROP TABLE messages CASCADE;

        ALTER TABLE messages_v2 RENAME TO messages;

        CREATE INDEX idx_messages_channel_id ON messages(channel_id, created_at DESC);
        CREATE INDEX idx_messages_author_id ON messages(author_id);
        CREATE INDEX idx_messages_reply_to_id ON messages(reply_to_id);

        ALTER TABLE message_attachments DROP COLUMN IF EXISTS message_created_at;
        ALTER TABLE message_mentions DROP COLUMN IF EXISTS message_created_at;
    END IF;
END $$;
