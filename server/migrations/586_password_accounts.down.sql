-- Users without email must be assigned distinct addresses before downgrading.
ALTER TABLE "user"
    ADD CONSTRAINT user_email_key UNIQUE (email),
    DROP COLUMN email_verified,
    DROP COLUMN password_hash,
    DROP COLUMN username;
