-- +goose Up
-- 起因（MCPT-03，对外 MCP 的凭据，docs/mcp）：外部 AI 客户端（Claude / Cursor / opencode…）
-- 要以「某个用户在某个工作区里」的身份调用平台能力。不支持 OAuth 的客户端用访问密钥接入。
--
--   mcp_grant      —— 「用户 × 工作区 × 客户端」的授权：scope、工具集、「需页面确认」、状态、最近使用。
--                     ⚠️ 它**不存角色**：有效权限 = scope ∩ 用户在该工作区的**当前**角色，
--                     每次调用实时查成员表（MCP-F-11）。把角色冻在这里的话，成员被降级或移出
--                     之后他的密钥照样按原权限用到过期 —— 而后台看不出任何异常。
--                     访问密钥与（M3 的）OAuth 令牌都挂在 grant 下，撤销的单位是 grant。
--   mcp_access_key —— 访问密钥。⚠️ **只存哈希与前缀**（MCP-N-01）：明文只在创建响应里出现一次。
--                     哈希是**不加盐的 sha256**：密钥本身是 32 字节随机数（熵足够），而每把密钥
--                     各加随机盐就无法按哈希走唯一索引直查（开发 01 §12 R4）。将来若加全局 pepper，
--                     不得复用 SEEAITV_SECRET_KEY —— 密钥轮换会让所有访问密钥同时失效。
--                     expires_at 为空 = 永久（MCP-PARAM-05：默认 90 天、可选永久，设计建议值）。
create table mcp_grant (
  id                    bigint primary key,
  user_id               bigint not null,
  workspace_id          bigint not null,
  client_id             bigint,                        -- OAuth 客户端（MCPT-12）；访问密钥为空
  client_name           varchar(100) not null,         -- 展示与画布节点来源角标
  source                varchar(16) not null,          -- access_key | oauth
  scopes                text[] not null,
  toolsets              text[] not null,
  require_page_confirm  boolean not null default false,
  status                varchar(16) not null default 'active',
  last_used_at          timestamptz,
  created_at            timestamptz not null default now(),
  revoked_at            timestamptz,
  constraint ck_mcp_grant_source check (source in ('access_key','oauth')),
  constraint ck_mcp_grant_status check (status in ('active','revoked')),
  -- 撤销时间与状态必须一致：只改其一的话，「什么时候撤销的」与「撤没撤销」会给出两个答案。
  constraint ck_mcp_grant_revoked check ((status = 'revoked') = (revoked_at is not null))
);
create index idx_mcp_grant_user on mcp_grant (user_id, status);
create index idx_mcp_grant_workspace on mcp_grant (workspace_id, status);

create table mcp_access_key (
  id          bigint primary key,
  grant_id    bigint not null,
  prefix      varchar(16) not null,                    -- sk_mcp_ 之后的前 8 位明文，便于识别
  key_hash    bytea not null,
  expires_at  timestamptz,                             -- 空 = 永久
  created_at  timestamptz not null default now()
);
create unique index uk_mcp_access_key_hash on mcp_access_key (key_hash);
create index idx_mcp_access_key_grant on mcp_access_key (grant_id);

-- +goose Down
drop table mcp_access_key;
drop table mcp_grant;
