-- AI agent 管理网盘（见 internal/api/agent*.go）。
--
-- agent_tokens：网页上发给 AI 的令牌。两种：
--   kind = 'agent'：调 /api/agent/v1 和 /mcp，带权限范围（scopes）；只存令牌的 SHA-256，明文只在创建时显示一次
--   kind = 'link' ：上传链接 /u/<token>（只能往 root_folder_id 里传），令牌明文也存着，网页上可以再复制（和分享链接一样）
-- 上传链接原来在 drop_links 表（0015），这里搬过来，已经发出去的链接照样能用。
CREATE TABLE agent_tokens (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind           text NOT NULL CHECK (kind IN ('agent', 'link')),
    name           text NOT NULL DEFAULT '',
    token_hash     bytea NOT NULL UNIQUE,                           -- SHA-256(令牌)
    token          text UNIQUE,                                     -- 只有上传链接存明文
    last4          text NOT NULL DEFAULT '',
    scopes         text[] NOT NULL DEFAULT '{}',
    root_folder_id uuid REFERENCES files (id) ON DELETE CASCADE,    -- 只能碰这个文件夹（含子文件夹）；空 = 整个网盘
    allow_private  boolean NOT NULL DEFAULT false,                  -- 能不能看私密文件夹
    limits         jsonb NOT NULL DEFAULT '{}',                     -- 每天的额度，没写的用默认值
    issued_under   text NOT NULL DEFAULT '',                        -- 签发时的管理员令牌指纹：换了管理员令牌，agent 令牌全部作废
    expires_at     timestamptz,
    revoked_at     timestamptz,
    frozen_at      timestamptz,                                     -- 连续被拒太多次，自动冻结
    uploads        bigint NOT NULL DEFAULT 0,
    bytes          bigint NOT NULL DEFAULT 0,
    last_used_at   timestamptz,
    last_ip        text NOT NULL DEFAULT '',
    ips            text[] NOT NULL DEFAULT '{}',                    -- 用过的 IP（最多 20 个），新 IP 记一条审计
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (kind = 'agent' OR (token IS NOT NULL AND root_folder_id IS NOT NULL))
);
CREATE INDEX agent_tokens_user_idx ON agent_tokens (user_id, kind, created_at DESC);

INSERT INTO agent_tokens (id, user_id, kind, name, token_hash, token, last4, scopes, root_folder_id,
                          expires_at, uploads, bytes, last_used_at, created_at)
SELECT id, user_id, 'link', name, sha256(convert_to(token, 'UTF8')), token, right(token, 4), '{files:upload}', folder_id,
       expires_at, uploads, bytes, last_used_at, created_at
FROM drop_links;
DROP TABLE drop_links;

-- 每把令牌每天用了多少（额度）
CREATE TABLE agent_usage (
    token_id uuid NOT NULL REFERENCES agent_tokens (id) ON DELETE CASCADE,
    day      date NOT NULL,
    counter  text NOT NULL,
    n        bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (token_id, day, counter)
);

-- 审计日志：agent 的每个请求（包括被拒的）。不写进服务端日志（Actions 日志是公开的），只在网页上看。留 180 天。
CREATE TABLE agent_audit (
    id         bigserial PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_id   uuid REFERENCES agent_tokens (id) ON DELETE SET NULL,
    token_name text NOT NULL DEFAULT '',
    at         timestamptz NOT NULL DEFAULT now(),
    action     text NOT NULL,
    file_ids   uuid[] NOT NULL DEFAULT '{}',
    paths      text[] NOT NULL DEFAULT '{}',
    detail     jsonb NOT NULL DEFAULT '{}',   -- 改之前的名字、位置（撤销用）等
    result     text NOT NULL,                 -- ok | denied | error | pending
    code       text NOT NULL DEFAULT '',
    bytes      bigint NOT NULL DEFAULT 0,
    ip         text NOT NULL DEFAULT '',
    user_agent text NOT NULL DEFAULT '',
    batch_id   uuid,
    undone_at  timestamptz
);
CREATE INDEX agent_audit_user_idx ON agent_audit (user_id, at DESC);
CREATE INDEX agent_audit_token_idx ON agent_audit (token_id, at DESC);
CREATE INDEX agent_audit_batch_idx ON agent_audit (batch_id) WHERE batch_id IS NOT NULL;

-- 危险操作申请：agent 只能申请，网页上人确认（刷脸）后才执行；24 小时没批自动作废。
CREATE TABLE agent_requests (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_id   uuid REFERENCES agent_tokens (id) ON DELETE SET NULL,
    token_name text NOT NULL DEFAULT '',
    action     text NOT NULL,                 -- purge | empty_trash | unprivate | reconcile
    file_ids   uuid[] NOT NULL DEFAULT '{}',
    paths      text[] NOT NULL DEFAULT '{}',
    reason     text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'expired', 'failed')),
    result     text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz
);
CREATE INDEX agent_requests_user_idx ON agent_requests (user_id, status, created_at DESC);

-- 分享链接是哪把 agent 令牌建的（agent 只能看到、撤销自己建的）
ALTER TABLE shares ADD COLUMN agent_token_id uuid REFERENCES agent_tokens (id) ON DELETE SET NULL;
