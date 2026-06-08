-- Remove banner URL column from users table
ALTER TABLE users
    DROP COLUMN IF EXISTS banner_url;
