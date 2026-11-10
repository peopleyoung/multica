CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS user_username_unique_idx ON "user" (lower(username)) WHERE username IS NOT NULL;
