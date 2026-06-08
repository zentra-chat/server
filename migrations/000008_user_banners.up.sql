-- Add banner URL support for user profiles
ALTER TABLE users
    ADD COLUMN banner_url TEXT;
