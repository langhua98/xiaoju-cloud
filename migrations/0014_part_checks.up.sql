-- 定期检查：后台慢慢把分块从两个频道下载下来核对 sha256（坏了用另一份修好），记下上次检查的时间和发现的问题
ALTER TABLE file_parts ADD COLUMN verified_at timestamptz;
ALTER TABLE file_parts ADD COLUMN verify_error text;
CREATE INDEX file_parts_verified_idx ON file_parts (verified_at NULLS FIRST);
