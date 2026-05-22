-- Migration: 000014_messages_unpartition (DOWN)
-- Re-adds the dropped columns. The un-partitioning cannot be automatically
-- reversed. Restore from backup if rollback of the table structure is needed.

ALTER TABLE message_attachments ADD COLUMN IF NOT EXISTS message_created_at TIMESTAMPTZ;
ALTER TABLE message_mentions ADD COLUMN IF NOT EXISTS message_created_at TIMESTAMPTZ;
