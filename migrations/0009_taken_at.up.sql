-- 拍摄时间：照片的 EXIF、视频的元数据里读出来的（生成缩略图时一起读），相册按它排序；读不到时为 NULL，按上传时间排。
ALTER TABLE files ADD COLUMN taken_at timestamptz;

-- 缩略图是哪一版程序生成的：0 是加这一列之前的（没有读拍摄时间），服务启动后在后台重新生成一遍。
ALTER TABLE thumbnails ADD COLUMN version smallint NOT NULL DEFAULT 0;
