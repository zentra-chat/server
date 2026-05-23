-- Add per-tenant encrypted data encryption keys.
-- Each community and DM conversation gets its own 32-byte AES key,
-- wrapped with the server's key encryption key (KEK).

ALTER TABLE communities
    ADD COLUMN encrypted_dek BYTEA;

ALTER TABLE dm_conversations
    ADD COLUMN encrypted_dek BYTEA;

-- Down migration would drop these columns.
