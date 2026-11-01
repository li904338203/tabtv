-- outbox、审计与日志（数据模型 §13）
-- 起因：outbox 让「DB 提交成功但消息没发出去」变成可恢复状态（FLOW-02、GO-18）。
-- 三张日志表是仅有的无界增长表，必须有保留期清理。
-- +goose Up

create table outbox_message (
    id             bigint primary key,
    aggregate_type varchar(32) not null,
    aggregate_id   bigint      not null,
    topic          varchar(64) not null,
    payload        jsonb       not null,
    schema_version integer     not null default 1,
    published_at   timestamptz,
    attempt        integer     not null default 0,
    last_error     varchar(1024),
    created_at     timestamptz not null default now()
);
-- ⚠️ 部分索引只覆盖未发布行：表再大扫描也快。
create index idx_outbox_pending on outbox_message (created_at) where published_at is null;
create index idx_outbox_aggregate on outbox_message (aggregate_type, aggregate_id);

-- 业务审计。与请求日志分开：请求日志是"谁调了什么接口"，
-- 审计是"什么对象从什么值变成什么值、为什么"（GAP-24）。
create table audit_log (
    id           bigint primary key,
    actor_type   varchar(16) not null,
    actor_id     bigint,
    workspace_id bigint,
    action       varchar(64) not null,
    object_type  varchar(32) not null,
    object_id    bigint,
    before_value jsonb,
    after_value  jsonb,
    reason       varchar(255),
    trace_id     varchar(64),
    created_at   timestamptz not null default now(),
    constraint ck_audit_actor check (actor_type in ('user','admin','system'))
);
create index idx_audit_object on audit_log (object_type, object_id, created_at desc);
create index idx_audit_actor on audit_log (actor_type, actor_id, created_at desc);
create index idx_audit_trace on audit_log (trace_id) where trace_id is not null;

create table api_request_log (
    id           bigint primary key,
    trace_id     varchar(64) not null,
    method       varchar(8)  not null,
    path         varchar(512) not null,
    http_status  integer     not null,
    -- HTTP 状态码已与业务码一致（CD-01），biz_code 仍单列以便按错误类型统计。
    biz_code     integer,
    duration_ms  integer     not null,
    user_id      bigint,
    admin_id     bigint,
    workspace_id bigint,
    ip           varchar(64),
    -- 已脱敏 + 截断。内嵌 base64 替换为摘要：
    -- 一张内联参考图 base64 后数 MB，原样入库一条就能把表撑爆。
    request_body text,
    created_at   timestamptz not null default now()
);
create index idx_api_log_time on api_request_log (created_at);
create index idx_api_log_trace on api_request_log (trace_id);
create index idx_api_log_failed on api_request_log (created_at) where http_status >= 400;

create table provider_request_log (
    id             bigint primary key,
    trace_id       varchar(64),
    generation_id  bigint,
    provider_code  varchar(32) not null,
    model_code     varchar(64),
    operation_code varchar(32),
    url            varchar(512) not null,
    http_status    integer,
    -- 错误分类驱动重试决策：408/429/5xx 可重试，401/403/4xx 不可。
    error_type     varchar(16) not null default 'none',
    duration_ms    integer     not null,
    request_body   text,
    response_body  text,
    created_at     timestamptz not null default now(),
    constraint ck_provider_log_error check (error_type in
        ('none','timeout','connect','dns','http','stream','decode'))
);
create index idx_provider_log_gen on provider_request_log (generation_id) where generation_id is not null;
create index idx_provider_log_time on provider_request_log (created_at);
create index idx_provider_log_failed on provider_request_log (provider_code, created_at)
    where error_type <> 'none';

create table stat_daily (
    id         bigint primary key,
    stat_date  date        not null,
    metric     varchar(64) not null,
    dimension  varchar(64) not null default '',
    value      bigint      not null default 0,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);
create unique index uk_stat on stat_daily (stat_date, metric, dimension);

-- +goose Down
drop table if exists stat_daily;
drop table if exists provider_request_log;
drop table if exists api_request_log;
drop table if exists audit_log;
drop table if exists outbox_message;
