CREATE TABLE workspace_model (
    id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    name TEXT NOT NULL,
    base_url TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    api_key_encrypted BYTEA NOT NULL DEFAULT '',
    capabilities JSONB NOT NULL,
    is_default BOOLEAN NOT NULL DEFAULT false,
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
