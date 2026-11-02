-- 谷歌云盘双向同步（见 internal/gdrive）。
--
-- gdrive_accounts：网页上「连接谷歌云盘」授权后拿到的 refresh token（用主密钥加密），一个用户一个谷歌账号。
CREATE TABLE gdrive_accounts (
    user_id        uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    email          text NOT NULL DEFAULT '',
    refresh_sealed bytea NOT NULL,                 -- AES-GCM(主密钥, refresh token)
    page_token     text NOT NULL DEFAULT '',       -- changes 接口的位置：没有变化就不用整个对一遍
    error          text NOT NULL DEFAULT '',       -- 授权失效等（要重新连接）
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- gdrive_links：一对同步的文件夹。网盘这边的文件夹删掉（永久删除）时同步一起删掉。
CREATE TABLE gdrive_links (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    gdrive_id    text NOT NULL,
    gdrive_name  text NOT NULL DEFAULT '',
    folder_id    uuid NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    paused       boolean NOT NULL DEFAULT false,
    last_sync_at timestamptz,
    last_error   text NOT NULL DEFAULT '',
    last_result  jsonb NOT NULL DEFAULT '{}',
    fingerprint  text NOT NULL DEFAULT '',         -- 网盘这边上次对完时的样子（个数 + 最后修改时间）
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, gdrive_id),
    UNIQUE (folder_id)
);

-- gdrive_items：上次同步完时两边对应的条目。两边各自的名字、上级、内容都记着，用来判断这次是哪边改的。
CREATE TABLE gdrive_items (
    link_id    uuid NOT NULL REFERENCES gdrive_links (id) ON DELETE CASCADE,
    gdrive_id  text NOT NULL,
    file_id    uuid NOT NULL,                      -- 不加外键：网盘这边永久删除后这里留着，下次同步时发现、删掉
    is_folder  boolean NOT NULL,
    g_name     text NOT NULL,
    g_parent   text NOT NULL,
    g_md5      text NOT NULL DEFAULT '',
    x_name     text NOT NULL,
    x_parent   uuid NOT NULL,
    x_sha256   bytea,
    PRIMARY KEY (link_id, gdrive_id),
    UNIQUE (link_id, file_id)
);
