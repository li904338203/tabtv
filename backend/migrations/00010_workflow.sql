-- 整图 DAG（数据模型 §11.1，NEW-01~NEW-06）
-- 起因：旧系统没有整图调度器。graph_snapshot 固定运行时的图版本，
-- 让「运行期间编辑画布不影响本次运行」成为可验证的事实（NEW-03）。
-- +goose Up

create table workflow_run (
    id                 bigint primary key,
    workspace_id       bigint      not null,
    project_id         bigint      not null,
    payer_account_id   bigint      not null,
    created_by_user_id bigint      not null,
    -- 运行开始即固定图版本、参数与资源引用。没有它就回答不了
    -- 「这次运行到底执行的是什么」。
    graph_snapshot     jsonb       not null,
    target_node_ids    jsonb       not null default '[]'::jsonb,
    status             varchar(16) not null default 'pending',
    total_credits      integer     not null default 0,
    started_at         timestamptz,
    finished_at        timestamptz,
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now(),
    constraint ck_wf_status check (status in
        ('pending','running','success','partial_success','failed','cancelled'))
);
create index idx_wf_project on workflow_run (project_id, created_at desc);
create index idx_wf_recovery on workflow_run (status) where status in ('pending','running');

create table workflow_node_run (
    id            bigint primary key,
    run_id        bigint      not null,
    node_id       varchar(64) not null,
    depends_on    jsonb       not null default '[]'::jsonb,
    status        varchar(16) not null default 'pending',
    -- 复用 generation 而非自己调 Provider：重复消息与进程重启续跑
    -- 因此落到 generation 已有的幂等与 CAS 上，不重复扣费（NEW-05）。
    generation_id bigint,
    attempt       integer     not null default 0,
    credits_cost  integer     not null default 0,
    is_reused     boolean     not null default false,
    error_code    varchar(32),
    error_message varchar(1024),
    started_at    timestamptz,
    finished_at   timestamptz,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now(),
    -- blocked：上游被跳过或失败导致本节点无法执行（NEW-06）。
    constraint ck_wf_node_status check (status in
        ('pending','running','success','failed','skipped','blocked','cancelled'))
);
create unique index uk_wf_node on workflow_node_run (run_id, node_id);
-- 调度器查「依赖已满足的待执行节点」。
create index idx_wf_ready on workflow_node_run (run_id, status);

-- +goose Down
drop table if exists workflow_node_run;
drop table if exists workflow_run;
