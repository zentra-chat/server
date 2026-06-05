ALTER TABLE users DROP COLUMN IF EXISTS community_layout;

ALTER TABLE community_members
    ADD COLUMN position INTEGER DEFAULT 0 NOT NULL;

CREATE INDEX IF NOT EXISTS idx_community_members_user_position ON community_members(user_id, position);

UPDATE community_members cm
SET position = sub.row_num
FROM (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY joined_at) AS row_num
    FROM community_members
) sub
WHERE cm.id = sub.id;
