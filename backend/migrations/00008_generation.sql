-- 生成任务与产物（数据模型 §9）
-- 起因：状态机本轮做了两个决定——cancelled 独立成终态（CD-05）、
-- reviewing/rejected 移出 generation 改挂 work.review_status（CD-06）。
-- +goose Up

create table generation (
    id                 bigint primary key,
    workspace_id       bigint      not null,
    payer_account_id   bigint      not null,
    created_by_user_id bigint      not null,
    project_id         bigint      not null,
    node_id            varchar(64),
    group_item_id      bigint,
    shot_id            bigint,
    script_asset_id    bigint,
    operation_id       bigint      not null,
    model_id           bigint      not null,
    result_type        varchar(16) not null,
    -- 快照一旦写入不再变更：后台此后改模型/预设/参数定义，
    -- 都不得改变这次任务的输入解释（06 §3.1）。
    params_snapshot    jsonb       not null default '{}'::jsonb,
    inputs_snapshot    jsonb       not null default '[]'::jsonb,
    request_count      integer     not null default 1,
    credits_cost       integer     not null default 0,
    idempotency_key    varchar(128) not null,
    -- 重试是新建一行并指回原任务，不是把原记录改回 queued。
    retry_of_id        bigint,
    status             varchar(16) not null default 'queued',
    progress           integer     not null default 0,
    provider_task_id   varchar(256),
    attempt            integer     not null default 0,
    published_at       timestamptz,
    deadline_at        timestamptz,
    heartbeat_at       timestamptz,
    started_at         timestamptz,
    finished_at        timestamptz,
    error_code         varchar(32),
    error_message      varchar(1024),
    provider_raw_result jsonb,
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now(),
    -- ⚠️ 五态。cancelled 独立于 failed：用户主动取消不是故障，
    -- 混在一起会让成功率指标失真、且无法分别定退款规则（CD-05）。
    constraint ck_generation_status check (status in ('queued','running','success','failed','cancelled')),
    constraint ck_generation_progress check (progress between 0 and 100),
    constraint ck_generation_cost check (credits_cost >= 0)
);
-- 幂等受理：同用户同键重复/并发只产生一个任务和一次扣分（AC-04、FLOW-01）。
create unique index uk_generation_idem on generation (payer_account_id, idempotency_key);
-- Worker 领取。
create index idx_generation_claim on generation (status, created_at) where status = 'queued';
-- 超时恢复扫描。
create index idx_generation_recovery on generation (status, deadline_at)
    where status in ('queued','running');
-- ⚠️ 未发布补投（FLOW-02：提交后 API 崩溃，记录必须能被恢复扫描发现）。
create index idx_generation_unpublished on generation (created_at)
    where status = 'queued' and published_at is null;
-- 画布恢复：按节点取最近一次任务。
create index idx_generation_node on generation (project_id, node_id, created_at desc);
-- 任务中心。
create index idx_generation_ws on generation (workspace_id, status, created_at desc);
create index idx_generation_retry on generation (retry_of_id) where retry_of_id is not null;

-- 产物拆表而非 outputs jsonb：多产物选择、逐项授权下载、ZIP
-- 都要求按**单个产物**操作（GAP-12、NEW-15）。
create table generation_output (
    id              bigint primary key,
    generation_id   bigint      not null,
    idx             integer     not null,
    media_object_id bigint,
    output_type     varchar(16) not null,
    -- 文本产物没有媒体对象，直接存内容。
    text_content    text,
    meta            jsonb       not null default '{}'::jsonb,
    created_at      timestamptz not null default now(),
    constraint ck_output_type check (output_type in ('image','video','audio','text')),
    constraint ck_output_idx  check (idx >= 0),
    -- 非文本产物必须有媒体对象，否则就是"成功但取不到产物"。
    constraint ck_output_payload check (
        (output_type = 'text' and text_content is not null)
        or (output_type <> 'text' and media_object_id is not null))
);
create unique index uk_gen_output on generation_output (generation_id, idx);

-- +goose Down
drop table if exists generation_output;
drop table if exists generation;
