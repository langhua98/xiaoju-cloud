ALTER TABLE shares DROP COLUMN IF EXISTS agent_token_id;
DROP TABLE IF EXISTS agent_requests;
DROP TABLE IF EXISTS agent_audit;
DROP TABLE IF EXISTS agent_usage;

CREATE TABLE drop_links (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    folder_id    uuid NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    name         text NOT NULL DEFAULT '',
    token        text NOT NULL UNIQUE,
    expires_at   timestamptz,
    uploads      bigint NOT NULL DEFAULT 0,
    bytes        bigint NOT NULL DEFAULT 0,
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX drop_links_user_idx ON drop_links (user_id, created_at DESC);
INSERT INTO drop_links (id, user_id, folder_id, name, token, expires_at, uploads, bytes, last_used_at, created_at)
SELECT id, user_id, root_folder_id, name, token, expires_at, uploads, bytes, last_used_at, created_at
FROM agent_tokens WHERE kind = 'link' AND revoked_at IS NULL;
DROP TABLE IF EXISTS agent_tokens;
