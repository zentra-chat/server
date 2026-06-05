DROP INDEX IF EXISTS idx_community_members_user_position;
ALTER TABLE community_members DROP COLUMN IF EXISTS position;
