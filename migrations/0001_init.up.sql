-- 用户
CREATE TABLE users (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username   text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- 文件与目录（目录树）。真实内容不在这里，而在 file_parts 指向的存储里。
CREATE TABLE files (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    parent_id   uuid REFERENCES files (id) ON DELETE CASCADE,
    name        text NOT NULL,
    type        text NOT NULL CHECK (type IN ('file', 'folder')),
    size        bigint NOT NULL DEFAULT 0,
    mime        text NOT NULL DEFAULT '',
    sha256      bytea,                 -- 明文整文件哈希，后续用于秒传
    dek_wrapped bytea,                 -- 用主密钥包裹后的文件密钥 (AES-GCM)
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('uploading', 'active')),
    trashed_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- 同一目录下不允许重名（仅约束正常可见的条目）
CREATE UNIQUE INDEX files_unique_name ON files (
    user_id,
    COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid),
    name
) WHERE trashed_at IS NULL AND status = 'active';

CREATE INDEX files_parent_idx ON files (user_id, parent_id) WHERE trashed_at IS NULL;

-- 文件分块：每块对应 Telegram 频道里的一条消息（或本地驱动的一个文件）
CREATE TABLE file_parts (
    file_id    uuid NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    part_no    int NOT NULL,
    storage    text NOT NULL,          -- telegram | local
    channel_id bigint NOT NULL,
    message_id bigint NOT NULL,
    size       bigint NOT NULL,        -- CTR 模式下明文与密文等长
    iv         bytea NOT NULL,         -- AES-CTR 初始计数器 (16 字节)
    sha256     bytea NOT NULL,         -- 密文哈希，用于校验
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (file_id, part_no)
);

CREATE INDEX file_parts_location_idx ON file_parts (storage, channel_id, message_id);
