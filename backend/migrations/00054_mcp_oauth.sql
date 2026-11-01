-- +goose Up
-- 起因（MCPT-12，对外 MCP 的 OAuth 2.1 授权服务器，docs/mcp）：访问密钥（00049）要用户自己建好再
-- 粘贴进客户端配置；支持 OAuth 的客户端（Claude Desktop / ChatGPT 等）应当能「一键登录」——
-- 客户端发起授权 → 用户在 seeAitv 登录并在同意页选工作区与权限 → 客户端拿到令牌。
-- 令牌最终挂在同一张 mcp_grant 上（source='oauth'），权限仍是 scope ∩ **当前**成员角色，每次调用实时算。
--
--   mcp_client    —— 客户端登记。client_key 是 CIMD 的元数据文档 URL（规范优先）或 DCR 生成的 ID（兼容）；
--                    registration 区分两者：同意页据此显示「已验证来源（域名）」或「未验证的客户端」。
--                    CIMD 的 fetched_at 是缓存时间（24 小时懒刷新，拉取失败沿用旧缓存最多再 24 小时）。
--   mcp_auth_code —— 授权码。⚠️ **主键就是哈希**（sha256），库里没有明文；一次性（used_at 用 CAS 写）、
--                    5 分钟、绑定 client / redirect_uri / resource / PKCE challenge。
--                    重放（同一个码第二次出现）= 码泄漏了：撤销该 grant 下已签发的全部令牌。
--   mcp_token     —— 访问令牌（1 小时）与刷新令牌（30 天）。⚠️ **只存哈希**（与访问密钥同口径，MCP-N-01）。
--                    audience 是签发时的受保护资源（RFC 8707）：/mcp 的令牌不能拿去打 /mcp/x/script。
--                    刷新每次轮换：旧行 replaced_by 指向新行并作废；family_id 是同一次授权码兑换出来的
--                    整条链 —— 被轮换掉的刷新令牌超过宽限期又出现，视为泄漏，按 family 一次撤销。
--
-- 另加 mcp_grant 的部分唯一索引：同一用户在同一工作区对同一客户端**重复授权时更新同一条 grant**，
-- 不堆积（否则「已连接的客户端」里同一个客户端出现 N 次，撤销其中一条另外几条照样能用）。
-- 已撤销的 grant 不在索引里：撤销后再授权是一条新的 grant（历史保留）。
create table mcp_client (
  id            bigint primary key,
  client_key    varchar(512) not null,                 -- CIMD URL 或 DCR 生成的 client_id
  name          varchar(100) not null,
  redirect_uris text[] not null,
  registration  varchar(16) not null,                  -- cimd | dcr
  metadata      jsonb not null default '{}',
  fetched_at    timestamptz,                           -- CIMD 最近一次成功拉取；DCR 为空
  created_at    timestamptz not null default now(),
  constraint ck_mcp_client_reg check (registration in ('cimd','dcr'))
);
create unique index uk_mcp_client_key on mcp_client (client_key);

create table mcp_auth_code (
  code_hash      bytea primary key,
  grant_id       bigint not null,
  client_id      bigint not null,
  redirect_uri   varchar(1024) not null,
  code_challenge varchar(128) not null,
  resource       varchar(512) not null,
  expires_at     timestamptz not null,
  used_at        timestamptz,
  created_at     timestamptz not null default now()
);
create index idx_mcp_auth_code_grant on mcp_auth_code (grant_id);

create table mcp_token (
  id            bigint primary key,
  grant_id      bigint not null,
  family_id     bigint not null,                       -- 同一次授权码兑换出来的整条轮换链
  kind          varchar(8) not null,                   -- access | refresh
  token_hash    bytea not null,
  audience      varchar(512) not null,
  expires_at    timestamptz not null,
  replaced_by   bigint,                                -- 刷新轮换链：被轮换掉的旧行指向新行
  revoked_at    timestamptz,
  created_at    timestamptz not null default now(),
  constraint ck_mcp_token_kind check (kind in ('access','refresh')),
  -- 被轮换掉的一定也作废了：只写 replaced_by 不写 revoked_at 的话，旧刷新令牌照样能用。
  constraint ck_mcp_token_rotated check (replaced_by is null or revoked_at is not null)
);
create unique index uk_mcp_token_hash on mcp_token (token_hash);
create index idx_mcp_token_grant on mcp_token (grant_id);
create index idx_mcp_token_family on mcp_token (family_id);

alter table mcp_grant add constraint ck_mcp_grant_oauth_client
  check (source <> 'oauth' or client_id is not null);
create unique index uk_mcp_grant_oauth_client on mcp_grant (user_id, workspace_id, client_id)
  where status = 'active' and source = 'oauth';

-- +goose Down
drop index uk_mcp_grant_oauth_client;
alter table mcp_grant drop constraint ck_mcp_grant_oauth_client;
drop table mcp_token;
drop table mcp_auth_code;
drop table mcp_client;
