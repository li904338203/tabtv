-- 模型、Provider、能力矩阵与计价（数据模型 §6）
-- 起因：修 GAP-09（协议硬编码致管理端下拉漏项）与 GAP-22（规则层与协议层能力不一致）。
-- +goose Up

-- 协议注册表。启动时由代码里的 adapter registry 校验并 upsert is_registered：
-- 代码没实现的协议不可能出现在管理端下拉里，实现了的也不可能漏（修 GAP-09）。
create table api_protocol (
    code                 varchar(32) primary key,
    name                 varchar(64) not null,
    is_async             boolean     not null default false,
    requires_query_path  boolean     not null default false,
    is_registered        boolean     not null default false,
    created_at           timestamptz not null default now(),
    updated_at           timestamptz not null default now()
);

create table provider_config (
    id                  bigint primary key,
    code                varchar(32) not null,
    name                varchar(64) not null,
    adapter_type        varchar(32) not null,
    base_url            varchar(512) not null,
    auth_type           varchar(16) not null default 'bearer',
    -- 密文带 key 版本前缀，可轮换（internal/platform/secret）。
    api_key_cipher      text,
    key_version         integer     not null default 1,
    headers             jsonb       not null default '{}'::jsonb,
    test_path           varchar(256),
    connect_timeout_ms  integer     not null default 10000,
    request_timeout_ms  integer     not null default 120000,
    -- 流式调用的空闲看门狗：整体超时与"多久没有新数据"是两件事。
    -- 长文本可以合法地跑十分钟，但 60 秒不吐字就是死了。
    idle_timeout_ms     integer     not null default 60000,
    max_retries         integer     not null default 2,
    status              varchar(16) not null default 'disabled',
    version             integer     not null default 1,
    created_at          timestamptz not null default now(),
    updated_at          timestamptz not null default now(),
    constraint ck_provider_auth   check (auth_type in ('none','bearer','header')),
    constraint ck_provider_status check (status in ('enabled','disabled'))
);
create unique index uk_provider_code on provider_config (code);

create table operation (
    id             bigint primary key,
    code           varchar(32) not null,
    name           varchar(64) not null,
    domain         varchar(16) not null,
    in_node_types  varchar(128),
    out_node_type  varchar(32) not null,
    status         varchar(16) not null default 'enabled',
    sort           integer     not null default 0,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now(),
    constraint ck_operation_status check (status in ('enabled','disabled'))
);
create unique index uk_operation_code on operation (code);

create table model (
    id                  bigint primary key,
    code                varchar(64) not null,
    name                varchar(64) not null,
    provider_id         bigint      not null,
    domain              varchar(16) not null,
    sub_type            varchar(32),
    upstream_model_code varchar(128) not null,
    api_protocol        varchar(32) not null references api_protocol(code),
    submit_path         varchar(256),
    query_path          varchar(256),
    est_seconds         integer     not null default 30,
    -- 按字符不按 token：调用方手上只有字符串，token 数依分词器而异。
    max_input_chars     integer,
    min_member_level_id bigint,
    status              varchar(16) not null default 'disabled',
    sort                integer     not null default 0,
    created_at          timestamptz not null default now(),
    updated_at          timestamptz not null default now(),
    constraint ck_model_status check (status in ('enabled','disabled')),
    constraint ck_model_est    check (est_seconds > 0)
);
create unique index uk_model_code on model (code);
create index idx_model_provider on model (provider_id);

create table model_operation (
    id           bigint primary key,
    model_id     bigint  not null,
    operation_id bigint  not null,
    is_enabled   boolean not null default true,
    -- 操作级估时覆盖模型级：同一文本模型做纯文本润色十几秒、
    -- 做图片反推要几分钟，只有模型级 est 会让视觉理解请求撞超时墙。
    est_seconds  integer,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    constraint ck_model_op_est check (est_seconds is null or est_seconds > 0)
);
create unique index uk_model_op on model_operation (model_id, operation_id);

create table model_param_support (
    id             bigint primary key,
    model_id       bigint not null,
    param_def_id   bigint not null,
    allowed_values jsonb,
    default_value  varchar(255),
    override       jsonb,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now()
);
create unique index uk_model_param on model_param_support (model_id, param_def_id);

-- ⚠️ 能力矩阵（修 GAP-22）。规则层表达画布语义，本表表达"该型号真能收什么"。
-- 实例：通用 ref2video 允许 audio，但 Seedance 拒收；BBLABU 不支持 first_last_frame。
-- 校验顺序：连线规则 → 能力矩阵 → 参数范围 → 会员等级 → 计价 → 扣费（AC-10）。
create table model_input_capability (
    id           bigint primary key,
    model_id     bigint      not null,
    operation_id bigint      not null,
    input_role   varchar(32) not null,
    min_count    integer     not null default 0,
    max_count    integer     not null default 1,
    allowed_mime varchar(255),
    created_at   timestamptz not null default now(),
    constraint ck_cap_role  check (input_role in
        ('text','image','audio','video','first_frame','last_frame','reference')),
    constraint ck_cap_count check (min_count >= 0 and max_count >= min_count)
);
create unique index uk_model_cap on model_input_capability (model_id, operation_id, input_role);

create table pricing (
    id           bigint primary key,
    operation_id bigint      not null,
    -- 可空：null 表示该 operation 的兜底价。
    model_id     bigint,
    credits      integer     not null,
    unit         varchar(16) not null,
    status       varchar(16) not null default 'enabled',
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    constraint ck_pricing_unit   check (unit in ('per_time','per_image','per_second','per_kchar')),
    constraint ck_pricing_status check (status in ('enabled','disabled')),
    constraint ck_pricing_credits check (credits >= 0)
);
-- nulls not distinct：让 (op, null) 这条兜底价也受唯一约束保护。
create unique index uk_pricing on pricing (operation_id, model_id) nulls not distinct;

-- +goose Down
drop table if exists pricing;
drop table if exists model_input_capability;
drop table if exists model_param_support;
drop table if exists model_operation;
drop table if exists model;
drop table if exists operation;
drop table if exists provider_config;
drop table if exists api_protocol;
