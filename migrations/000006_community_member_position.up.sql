-- Add position column to community_members for per-user community ordering
ALTER TABLE community_members
    ADD COLUMN position INTEGER DEFAULT 0 NOT NULL;

-- Set initial positions based on join order for existing members
UPDATE community_members cm
SET position = sub.row_num
FROM (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY joined_at) AS row_num
    FROM community_members
) sub
WHERE cm.id = sub.id;

CREATE INDEX IF NOT EXISTS idx_community_members_user_position ON community_members(user_id, position);
