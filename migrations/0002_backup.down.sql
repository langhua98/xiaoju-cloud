DROP INDEX IF EXISTS file_parts_backup_missing_idx;
ALTER TABLE file_parts
    DROP COLUMN IF EXISTS backup_channel_id,
    DROP COLUMN IF EXISTS backup_message_id;
