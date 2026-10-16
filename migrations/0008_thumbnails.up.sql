-- 缩略图（相册网格用）：320×320 的 JPEG，用主密钥加密后直接存在数据库里（每张约 15 KB），
-- 不占 Telegram 的发送频率。文件永久删除时一起删掉；随时可以重新生成，不属于需要备份的数据。
CREATE TABLE thumbnails (
    file_id     uuid PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
    data        bytea NOT NULL,
    duration_ms bigint NOT NULL DEFAULT 0, -- 视频时长
    created_at  timestamptz NOT NULL DEFAULT now()
);
