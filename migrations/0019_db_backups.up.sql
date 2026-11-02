-- 数据库自动备份（见 internal/service/dbbackup.go）：每次备份一行。
-- 备份文件存进网盘「数据库备份」文件夹，同时直接发进两个 Telegram 频道并置顶（数据库丢了也能找到），
-- 这里记下消息 ID，旧的备份按数量删掉。
CREATE TABLE db_backups (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at     timestamptz NOT NULL DEFAULT now(),
    size           bigint NOT NULL,
    sha256         bytea NOT NULL,              -- 备份文件（加密后）的摘要
    migration      text NOT NULL DEFAULT '',    -- 备份时最新的迁移
    file_id        uuid,                        -- 网盘里那个文件（删掉了也留着这一行）
    main_message   bigint,                      -- 主频道里的消息
    backup_message bigint,                      -- 备份频道里的消息
    error          text NOT NULL DEFAULT ''     -- 有一处没存成功时的原因
);
CREATE INDEX db_backups_created_idx ON db_backups (created_at DESC);
