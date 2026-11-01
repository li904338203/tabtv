-- 身份、会话与权限（数据模型 §3）
-- 起因：新库从 V1 直接建最终结构，不重放旧 Flyway 的 V1~V60。
-- app_user 不叫 user：user 是 PG 保留字，旧系统被迫全程加引号。
-- +goose Up

create table app_user (
    id                   bigint primary key,
    phone                varchar(32),
    email                varchar(255),
    password_hash        varchar(100)  not null,
    nickname             varchar(64)   not null,
    avatar_media_id      bigint,
    is_ai_watermark_off  boolean       not null default false,
    status               varchar(16)   not null default 'active',
    -- token_version 是会话撤销的快路径：JWT 校验时比对即可，无需查表。
    -- 改密/封禁时 +1，一次性作废该用户全部已签发 token（修 GAP-19）。
    token_version        integer       not null default 0,
    last_login_at        timestamptz,
    created_at           timestamptz   not null default now(),
    updated_at           timestamptz   not null default now(),
    constraint ck_app_user_status  check (status in ('active','banned')),
    -- phone / email 至少有一项，否则账号无法登录也无法找回。
    constraint ck_app_user_account check (phone is not null or email is not null)
);
-- 部分唯一索引：允许多行为 null，但非 null 值必须唯一。
create unique index uk_app_user_phone on app_user (phone) where phone is not null;
create unique index uk_app_user_email on app_user (email) where email is not null;

create table admin_user (
    id             bigint primary key,
    username       varchar(64)  not null,
    password_hash  varchar(100) not null,
    status         varchar(16)  not null default 'active',
    token_version  integer      not null default 0,
    last_login_at  timestamptz,
    created_at     timestamptz  not null default now(),
    updated_at     timestamptz  not null default now(),
    constraint ck_admin_user_status check (status in ('active','disabled'))
);
create unique index uk_admin_user_username on admin_user (username);

-- 会话表。修 GAP-19：旧系统 logout 只返回成功、refresh 不查用户状态，
-- 封禁/改密后旧 token 仍可用。
create table auth_session (
    id             bigint primary key,
    jti            varchar(64)  not null,
    actor_kind     varchar(16)  not null,
    actor_id       bigint       not null,
    issued_at      timestamptz  not null default now(),
    expires_at     timestamptz  not null,
    revoked_at     timestamptz,
    revoked_reason varchar(32),
    last_seen_at   timestamptz,
    ip             varchar(64),
    user_agent     varchar(512),
    constraint ck_auth_session_kind   check (actor_kind in ('user','admin')),
    constraint ck_auth_session_reason check (revoked_reason is null or revoked_reason in
        ('logout','password_changed','banned','admin_revoked','expired'))
);
create unique index uk_auth_session_jti on auth_session (jti);
create index idx_auth_session_actor on auth_session (actor_kind, actor_id, revoked_at);
-- 过期会话清理扫描用。
create index idx_auth_session_expiry on auth_session (expires_at) where revoked_at is null;

create table permission (
    id          bigint primary key,
    code        varchar(64)  not null,
    name        varchar(64)  not null,
    module      varchar(32)  not null,
    created_at  timestamptz  not null default now()
);
-- permission.code 是后端鉴权与前端路由 meta 的共同契约，不得随意改名。
create unique index uk_permission_code on permission (code);

create table role (
    id          bigint primary key,
    code        varchar(64)  not null,
    name        varchar(64)  not null,
    description varchar(255),
    created_at  timestamptz  not null default now(),
    updated_at  timestamptz  not null default now()
);
create unique index uk_role_code on role (code);

create table role_permission (
    id            bigint primary key,
    role_id       bigint not null,
    permission_id bigint not null
);
create unique index uk_role_permission on role_permission (role_id, permission_id);

create table admin_user_role (
    id            bigint primary key,
    admin_user_id bigint not null,
    role_id       bigint not null
);
create unique index uk_admin_user_role on admin_user_role (admin_user_id, role_id);

-- 认证渠道与发送配置。单行配置，密文带 key 版本前缀（可轮换）。
create table auth_setting (
    id                    bigint primary key,
    is_phone_enabled      boolean     not null default true,
    is_email_enabled      boolean     not null default true,
    is_register_code_required boolean not null default true,
    smtp_config           jsonb       not null default '{}'::jsonb,
    sms_config            jsonb       not null default '{}'::jsonb,
    key_version           integer     not null default 1,
    version               integer     not null default 1,
    updated_by_admin_id   bigint,
    created_at            timestamptz not null default now(),
    updated_at            timestamptz not null default now()
);

-- +goose Down
drop table if exists auth_setting;
drop table if exists admin_user_role;
drop table if exists role_permission;
drop table if exists role;
drop table if exists permission;
drop table if exists auth_session;
drop table if exists admin_user;
drop table if exists app_user;
