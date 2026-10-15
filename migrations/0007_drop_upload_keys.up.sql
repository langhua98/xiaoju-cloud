-- 批量上传改为不需要密钥（只能往“相册”里添加，不能查看、下载或删除），上传密钥表不再使用。
DROP TABLE IF EXISTS upload_keys;
