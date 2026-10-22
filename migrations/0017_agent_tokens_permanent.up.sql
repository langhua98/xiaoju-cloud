-- 2026-10-22 William 要 AI 令牌永久不过期：还在用的（没撤销、没过期）一律改成永久。已经过期的不复活。
UPDATE agent_tokens SET expires_at = NULL
WHERE kind = 'agent' AND revoked_at IS NULL AND expires_at IS NOT NULL AND expires_at > now();
