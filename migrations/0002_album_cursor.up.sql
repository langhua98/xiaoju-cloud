-- 快捷指令相册同步的进度：照片按拍摄时间从旧到新上传，这里记下已经连续备份到哪个拍摄时间
-- （手机本地时间，yyyyMMdd_HHmmss），下次运行只查这之后的照片。
CREATE TABLE album_cursors (
    user_id    uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    stamp      text COLLATE "C" NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
