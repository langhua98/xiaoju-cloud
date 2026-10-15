-- 被强制退出的会话（如看照片时格子口令答错）。会话凭证本身无状态，
-- 所以要记下作废的会话 ID，直到它自然过期；服务重启后依然有效。
CREATE TABLE revoked_sessions (
    id         text PRIMARY KEY,
    expires_at timestamptz NOT NULL
);
