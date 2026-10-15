-- 上传密钥：给 iPhone 快捷指令等批量上传用，只能往“相册”文件夹里添加文件，
-- 不能列目录、下载或删除（看照片仍然要刷脸、格子口令）。只存哈希，密钥本身只在创建时显示一次。
CREATE TABLE upload_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL,
    key_hash     bytea NOT NULL UNIQUE,
    uploads      bigint NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
