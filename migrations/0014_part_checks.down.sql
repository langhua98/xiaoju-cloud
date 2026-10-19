DROP INDEX IF EXISTS file_parts_verified_idx;
ALTER TABLE file_parts DROP COLUMN IF EXISTS verify_error;
ALTER TABLE file_parts DROP COLUMN IF EXISTS verified_at;
