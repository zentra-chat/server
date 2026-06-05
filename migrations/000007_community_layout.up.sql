-- Replace community_members.position with user-level JSONB layout
-- This enables future extension with folders, pinning, etc.

-- Drop the per-member position approach
DROP INDEX IF EXISTS idx_community_members_user_position;
ALTER TABLE community_members DROP COLUMN IF EXISTS position;

-- Add user-level community layout (JSONB for forward-compat with folders)
ALTER TABLE users
    ADD COLUMN community_layout JSONB NOT NULL DEFAULT '{"communityOrder":[]}'::jsonb;

-- Backfill: set community_order based on existing join order for all users
WITH ordered AS (
    SELECT user_id, array_agg(community_id ORDER BY joined_at) AS ordered_ids
    FROM community_members
    GROUP BY user_id
)
UPDATE users u
SET community_layout = jsonb_build_object('communityOrder', o.ordered_ids)
FROM ordered o
WHERE u.id = o.user_id;
