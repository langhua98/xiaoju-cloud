-- 已注册的 Face ID / 指纹设备（通行密钥）。只保存公钥等凭据信息，不保存任何生物特征数据。
CREATE TABLE passkeys (
    id           bytea PRIMARY KEY,      -- 凭据 ID
    name         text NOT NULL,          -- 设备名，便于识别和删除
    credential   jsonb NOT NULL,         -- go-webauthn 的 Credential（公钥、签名计数等）
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
