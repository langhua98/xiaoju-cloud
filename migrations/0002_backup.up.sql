-- 备份频道里的分块位置；为空表示还没有备份（后台任务会补齐）
ALTER TABLE file_parts
    ADD COLUMN backup_channel_id bigint,
    ADD COLUMN backup_message_id bigint;

CREATE INDEX file_parts_backup_missing_idx ON file_parts (created_at) WHERE backup_message_id IS NULL;
