-- 断点续传：大文件分好几次请求传，断网、刷新页面后从断的地方接着传。
-- 文件名、目录、密钥在 files 里那条 status = 'uploading' 的记录上，传好的分块在 file_parts 里。
CREATE TABLE upload_sessions (
    file_id    uuid PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    size       bigint NOT NULL,           -- 整个文件多大
    probe      text NOT NULL,             -- 文件开头、结尾各 1MB 的 SHA-256：再选同名同大小的文件时确认是同一个
    dedupe     boolean NOT NULL DEFAULT false, -- 传完时网盘里已有内容一样的就不存
    committed  bigint NOT NULL DEFAULT 0, -- 已经写进存储端的分块加起来多大（服务重启后从这里接着传）
    hash_state bytea,                     -- 读到 committed 为止整个文件 SHA-256 的中间状态
    updated_at timestamptz NOT NULL DEFAULT now()
);
