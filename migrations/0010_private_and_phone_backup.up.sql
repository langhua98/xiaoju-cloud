-- 私密文件夹：网页上默认把封面和缩略图遮住，验证（刷脸 / 格子口令）后才显示，里面的子文件夹也一样。
-- 隐藏相册、私密相册默认私密。
ALTER TABLE files ADD COLUMN private boolean NOT NULL DEFAULT false;
UPDATE files SET private = true
WHERE type = 'folder' AND parent_id IS NULL AND name IN ('隐藏相册', '私密相册') AND trashed_at IS NULL;

-- 快捷指令最近一次问“哪些还没传过”（POST /api/ingest/plan）时的清单：手机上有几个、当时网盘里已有几个。
-- 网页的“备份状态”据此显示手机上的照片备份到哪一步了。by_date：true 是从相册直接读（自动备份新照片），
-- false 是从「文件」里的文件夹传（备份照片）。
CREATE TABLE ingest_checks (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    folder     text NOT NULL,
    by_date    boolean NOT NULL,
    checked_at timestamptz NOT NULL DEFAULT now(),
    files      int NOT NULL,
    uploaded   int NOT NULL,
    todo       int NOT NULL,
    PRIMARY KEY (user_id, folder, by_date)
);
