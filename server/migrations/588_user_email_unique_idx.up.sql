CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS user_email_unique_idx ON "user" (email) WHERE email <> '';
