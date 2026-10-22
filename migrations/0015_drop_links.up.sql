-- 上传链接（给 AI agent 等用）：知道链接的人不用登录就能往一个文件夹里传文件，不能列目录、下载或删除。
-- 可以设有效期，随时撤销；文件夹永久删除时链接跟着删。
CREATE TABLE drop_links (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    folder_id    uuid NOT NULL REFERENCES files (id) ON DELETE CASCADE, -- 传进哪个文件夹
    name         text NOT NULL DEFAULT '',                               -- 备注（比如给哪个 agent 用）
    token        text NOT NULL UNIQUE,                                   -- 链接里的随机串
    expires_at   timestamptz,                                            -- 空表示不过期
    uploads      bigint NOT NULL DEFAULT 0,
    bytes        bigint NOT NULL DEFAULT 0,
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX drop_links_user_idx ON drop_links (user_id, created_at DESC);
