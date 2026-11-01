-- 项目与文件夹（数据模型 §5）
-- 起因：version 是服务端乐观锁，与 canvas_data.schemaVersion 是两件事，不可混用（GO-36）。
-- +goose Up

create table folder (
    id           bigint primary key,
    workspace_id bigint      not null,
    name         varchar(64) not null,
    sort         integer     not null default 0,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now()
);
-- 同一工作区不许重名：文件夹靠名字辨认，重名会让「移动至」菜单无从选择。
create unique index uk_folder_ws_name on folder (workspace_id, name);

create table project (
    id                 bigint primary key,
    workspace_id       bigint      not null,
    created_by_user_id bigint      not null,
    name               varchar(100) not null,
    cover_media_id     bigint,
    folder_id          bigint,
    canvas_data        jsonb       not null default '{}'::jsonb,
    -- 乐观锁。保存只允许 where version = ?，0 行更新即冲突（AC-03）。
    version            integer     not null default 1,
    status             varchar(16) not null default 'active',
    deleted_at         timestamptz,
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now(),
    constraint ck_project_status check (status in ('active','deleted'))
);
create index idx_project_ws on project (workspace_id, status, updated_at desc);
create index idx_project_folder on project (folder_id) where folder_id is not null;
-- ⚠️ 回收站清理扫描。保留期算 deleted_at：恢复/改名/移动都会刷 updated_at，
-- 用它算的话「删了又恢复又删」的项目永远清不掉。
create index idx_project_trash on project (status, deleted_at) where status = 'deleted';

-- +goose Down
drop table if exists project;
drop table if exists folder;
