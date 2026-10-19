-- 分享链接：知道链接（设了密码的还要密码）的人不用登录就能看一个文件，或一个文件夹（连同子文件夹）里的照片，可以设有效期
CREATE TABLE shares (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    file_id       uuid NOT NULL REFERENCES files (id) ON DELETE CASCADE, -- 分享的文件或文件夹（永久删除时这条跟着删）
    token         text NOT NULL UNIQUE,                                   -- 链接里的随机串
    password_hash bytea,                                                  -- scrypt；空表示不用密码
    password_salt bytea,
    expires_at    timestamptz,                                            -- 空表示不过期
    views         bigint NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX shares_user_idx ON shares (user_id, created_at DESC);
