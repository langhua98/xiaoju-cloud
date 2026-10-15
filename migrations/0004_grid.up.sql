-- 格子口令：登录时页面出一张随机 4×4 数字格，按主人设定的规则算出答案。
-- 规则（取哪两格、周一加几）用主密钥 AES-GCM 加密后存放，代码和数据库明文里都看不到。
CREATE TABLE grid_rule (
    id           int PRIMARY KEY DEFAULT 1 CHECK (id = 1), -- 只有一个管理员，只存一份
    rule_wrapped bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- 恢复码：每个只能用一次，可以代替“刷脸”或“格子口令”中的一步。只存哈希。
CREATE TABLE recovery_codes (
    code_hash  bytea PRIMARY KEY,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
