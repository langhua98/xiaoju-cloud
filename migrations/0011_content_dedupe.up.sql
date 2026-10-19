-- 按内容去重：内容相同（明文 SHA-256 相同）的文件已经在网盘里，就不再存一份
CREATE INDEX files_sha256_idx ON files (user_id, sha256)
    WHERE type = 'file' AND status = 'active' AND trashed_at IS NULL;

-- 快捷指令传来的文件按内容认出来已经有了、没有再存时，记下手机上的名字和大小。之后的上传计划
-- （按名字 + 大小 / 拍摄日期比对）直接当作传过，不用每次运行都再发一遍（比如照片在网盘里改过名、挪了文件夹）
CREATE TABLE content_aliases (
    folder_id  uuid NOT NULL REFERENCES files (id) ON DELETE CASCADE, -- 上传的目标相册
    name       text NOT NULL,
    size       bigint NOT NULL,
    file_id    uuid NOT NULL REFERENCES files (id) ON DELETE CASCADE, -- 内容相同的那个文件（永久删除时这条跟着删）
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (folder_id, name, size)
);
