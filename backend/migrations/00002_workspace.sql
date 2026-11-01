-- 工作区、成员与共享（数据模型 §4）
-- 起因：10 §4.5 要求「不能继续把所有资源只绑定单个 user_id」。
-- 一切业务资源挂 workspace_id；created_by_user_id 只记"谁干的"，不表示归属。
-- +goose Up

-- 计费主体与协作主体分表：工作区可改名/转让/归档，而账本必须保持连续可审计。
create table payer_account (
    id         bigint primary key,
    kind       varchar(16) not null,
    owner_id   bigint      not null,
    status     varchar(16) not null default 'active',
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    constraint ck_payer_kind   check (kind in ('user','workspace')),
    constraint ck_payer_status check (status in ('active','frozen'))
);
create unique index uk_payer_owner on payer_account (kind, owner_id);

create table workspace (
    id               bigint primary key,
    name             varchar(64) not null,
    kind             varchar(16) not null,
    owner_user_id    bigint      not null,
    payer_account_id bigint      not null,
    status           varchar(16) not null default 'active',
    deleted_at       timestamptz,
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    constraint ck_workspace_kind   check (kind in ('personal','team')),
    constraint ck_workspace_status check (status in ('active','deleted'))
);
create index idx_workspace_owner on workspace (owner_user_id, status);
-- 每个用户只能有一个个人工作区；注册时自动创建。
create unique index uk_workspace_personal on workspace (owner_user_id)
    where kind = 'personal' and status = 'active';

create table workspace_member (
    workspace_id bigint      not null,
    user_id      bigint      not null,
    id           bigint primary key,
    role         varchar(16) not null,
    status       varchar(16) not null default 'active',
    invited_by   bigint,
    joined_at    timestamptz not null default now(),
    removed_at   timestamptz,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    constraint ck_ws_member_role   check (role in ('owner','admin','editor','viewer')),
    constraint ck_ws_member_status check (status in ('active','removed'))
);
create unique index uk_ws_member on workspace_member (workspace_id, user_id);
-- 越权校验的主查询：当前用户属于哪些工作区。
create index idx_ws_member_user on workspace_member (user_id, status);

create table workspace_invitation (
    id                  bigint primary key,
    workspace_id        bigint      not null,
    target_kind         varchar(16) not null,
    target_value        varchar(255) not null,
    role                varchar(16) not null,
    -- 只存哈希：邀请链接一旦泄漏（日志、截图）明文即可被直接使用。
    token_hash          varchar(64) not null,
    status              varchar(16) not null default 'pending',
    expires_at          timestamptz not null,
    accepted_by_user_id bigint,
    accepted_at         timestamptz,
    invited_by          bigint      not null,
    created_at          timestamptz not null default now(),
    constraint ck_invite_target check (target_kind in ('email','phone')),
    constraint ck_invite_role   check (role in ('admin','editor','viewer')),
    constraint ck_invite_status check (status in ('pending','accepted','revoked','expired'))
);
create unique index uk_invite_token on workspace_invitation (token_hash);
create index idx_invite_ws on workspace_invitation (workspace_id, status);

create table resource_share (
    id            bigint primary key,
    workspace_id  bigint      not null,
    resource_type varchar(32) not null,
    resource_id   bigint      not null,
    grantee_type  varchar(16) not null,
    grantee_id    bigint      not null,
    permissions   varchar(128) not null,
    granted_by    bigint      not null,
    revoked_at    timestamptz,
    created_at    timestamptz not null default now(),
    constraint ck_share_resource check (resource_type in ('project','script_asset','timeline')),
    constraint ck_share_grantee  check (grantee_type in ('user','workspace'))
);
-- 同一对象对同一受让方只能有一条**有效**授权；撤销后可重新授权。
create unique index uk_share_active on resource_share (resource_type, resource_id, grantee_type, grantee_id)
    where revoked_at is null;
create index idx_share_grantee on resource_share (grantee_type, grantee_id) where revoked_at is null;

-- +goose Down
drop table if exists resource_share;
drop table if exists workspace_invitation;
drop table if exists workspace_member;
drop table if exists workspace;
drop table if exists payer_account;
