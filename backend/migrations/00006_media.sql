-- 媒体对象与引用保护（数据模型 §7）
-- 起因：旧系统把 object key 塞在 generation.outputs JSON 里，
-- 导致每个领域各自拼 URL、没有统一的 checksum/清理入口（GAP-17）。
-- +goose Up

create table media_object (
    id                 bigint primary key,
    workspace_id       bigint      not null,
    bucket             varchar(64) not null,
    object_key         varchar(512) not null,
    content_type       varchar(128) not null,
    size_bytes         bigint      not null,
    checksum           varchar(128),
    width              integer,
    height             integer,
    duration_ms        integer,
    purpose            varchar(32) not null,
    created_by_user_id bigint      not null,
    status             varchar(16) not null default 'active',
    deleted_at         timestamptz,
    created_at         timestamptz not null default now(),
    constraint ck_media_purpose check (purpose in
        ('upload_reference','generation_output','render_output','script_asset','banner','avatar','material_cover')),
    constraint ck_media_status  check (status in ('active','deleted')),
    constraint ck_media_size    check (size_bytes >= 0)
);
create unique index uk_media_key on media_object (bucket, object_key);
create index idx_media_ws on media_object (workspace_id, purpose, created_at desc);
-- ⚠️ 清理作业的判据是「无 usage 行 且 超保留期」，保留期算 deleted_at 不算 updated_at。
create index idx_media_cleanup on media_object (status, deleted_at) where deleted_at is not null;

-- 活动引用保护。删除被进行中任务引用的资源必须返回 40906，不是静默成功。
create table media_usage (
    id              bigint primary key,
    media_object_id bigint      not null,
    usage_type      varchar(32) not null,
    usage_ref_id    bigint      not null,
    created_at      timestamptz not null default now()
);
create unique index uk_media_usage on media_usage (media_object_id, usage_type, usage_ref_id);
create index idx_media_usage_ref on media_usage (usage_type, usage_ref_id);

create table upload_reference (
    id              bigint primary key,
    workspace_id    bigint      not null,
    project_id      bigint      not null,
    node_id         varchar(64) not null,
    media_object_id bigint      not null,
    media_type      varchar(16) not null,
    status          varchar(16) not null default 'active',
    created_by_user_id bigint   not null,
    created_at      timestamptz not null default now(),
    constraint ck_upload_type   check (media_type in ('image','audio','video')),
    constraint ck_upload_status check (status in ('active','deleted'))
);
create index idx_upload_node on upload_reference (project_id, node_id, status);

-- +goose Down
drop table if exists upload_reference;
drop table if exists media_usage;
drop table if exists media_object;
