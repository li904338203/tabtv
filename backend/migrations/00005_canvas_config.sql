-- 画布配置目录（数据模型 §5）
-- 起因：配置驱动画布。前端遍历配置渲染控件，不写死表单。
-- config_revision 解决旧系统"漏删缓存致前端最长 30 分钟拿旧配置"的问题。
-- +goose Up

create table node_type (
    id         bigint primary key,
    code       varchar(32) not null,
    name       varchar(64) not null,
    kind       varchar(16) not null,
    icon       varchar(64),
    sort       integer     not null default 0,
    is_enabled boolean     not null default true,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    constraint ck_node_kind check (kind in ('generator','material','composite'))
);
create unique index uk_node_type_code on node_type (code);

create table param_def (
    id           bigint primary key,
    code         varchar(64) not null,
    name         varchar(64) not null,
    control_type varchar(32) not null,
    config       jsonb       not null default '{}'::jsonb,
    is_enabled   boolean     not null default true,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    constraint ck_param_control check (control_type in
        ('select','segmented','slider','grid','tag_group','switch','dialog','button','upload','inline','text'))
);
create unique index uk_param_def_code on param_def (code);

create table node_param (
    id              bigint primary key,
    node_type_id    bigint      not null,
    param_def_id    bigint      not null,
    param_group     varchar(32),
    sort            integer     not null default 0,
    is_required     boolean     not null default false,
    default_value   varchar(255),
    config_override jsonb,
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now()
);
create unique index uk_node_param on node_param (node_type_id, param_def_id);

create table connection_rule (
    id                bigint primary key,
    source_type       varchar(32) not null,
    target_type       varchar(32) not null,
    is_allowed        boolean     not null default true,
    default_operation varchar(32),
    created_at        timestamptz not null default now(),
    updated_at        timestamptz not null default now()
);
create unique index uk_conn_rule on connection_rule (source_type, target_type);

create table image_preset (
    id           bigint primary key,
    code         varchar(64) not null,
    name         varchar(64) not null,
    preset_group varchar(32) not null,
    spec         varchar(32) not null,
    input_mode   varchar(32) not null,
    icon         varchar(64),
    -- 提示词在服务端拼接：前端拼出来的词会落进 canvas_data，
    -- 用户改一个字玩法就失效，调词也没法只靠迁移完成。
    prompt       text,
    params       jsonb       not null default '{}'::jsonb,
    dimensions   jsonb,
    operation    varchar(32),
    sort         integer     not null default 0,
    is_enabled   boolean     not null default true,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    constraint ck_preset_group check (preset_group in ('sheet','tuning','storyboard','space')),
    constraint ck_preset_spec  check (spec in ('sheet','sequence','variation','retouch','panorama')),
    constraint ck_preset_input check (input_mode in ('image_required','text_required','either'))
);
-- ⚠️ 下线预设用 is_enabled=false **不删行**：历史节点 params 里的 code 需保持可追溯。
create unique index uk_preset_code on image_preset (code);

-- 配置版本。写配置即 bump，配置接口带 revision 返回，缓存键含 revision。
-- 漏删缓存不再可能产生脏读（CD-14）。
create table config_revision (
    scope               varchar(32) primary key,
    revision            bigint      not null default 1,
    updated_by_admin_id bigint,
    updated_at          timestamptz not null default now()
);

create table project_template (
    id          bigint primary key,
    code        varchar(32) not null,
    name        varchar(64) not null,
    description varchar(255),
    canvas_data jsonb       not null,
    sort        integer     not null default 0,
    is_enabled  boolean     not null default true,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now()
);
create unique index uk_template_code on project_template (code);

-- +goose Down
drop table if exists project_template;
drop table if exists config_revision;
drop table if exists image_preset;
drop table if exists connection_rule;
drop table if exists node_param;
drop table if exists param_def;
drop table if exists node_type;
