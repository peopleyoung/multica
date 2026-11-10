ALTER TABLE "user"
    ADD COLUMN username TEXT,
    ADD COLUMN password_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN email_verified BOOLEAN NOT NULL DEFAULT true,
    DROP CONSTRAINT user_email_key;
