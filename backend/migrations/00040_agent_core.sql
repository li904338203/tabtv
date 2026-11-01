-- 画布 Agent 核心表（AGT-21；AG-04/05/06/08/11/15/19，docs/agent dev/02 §2.1）
--
-- 起因：Agent 一轮可能持续数分钟，中途要等用户（卡片）或等生成（任务），
-- 进程重启、浏览器关闭之后都必须能接着跑 —— 挂起与恢复只能落库，不能放内存（dev/01 T-4）。
-- 会话是运行单元；轮次是计费单位（refType=agent_turn，T-8）；步骤是每次工具调用；
-- 卡片是挂起点；额度预留表让并发的付费工具不会超出用户批准的额度（T-9）。
--
-- ⚠️ 两列幂等键是 dev/02 草案之后补的：`POST /agent/sessions` 与 `POST .../messages`
-- 都要求 Idempotency-Key，唯一性由数据库兜底，不靠 Redis（同 uk_generation_idem）。
--   - agent_session.create_idempotency_key：作用域工作区（同 00039 uk_project_create_idem）；
--   - agent_turn.idempotency_key：作用域会话。
--   部分唯一索引：系统唤醒的轮次、不带键的请求全是 null，不该互相撞。
--
-- ⚠️ agent_message.seq 是会话内连续序号，靠**锁住会话行**后取 max+1 生成；
--    uk_agent_message_seq 是兜底，不是正常路径（不锁的话并发时报 23505，看起来像随机失败）。
-- ⚠️ agent_budget_entry 一个步骤一行（uk_agent_budget_step）：同一步骤重试换新步骤 ID，
--    不会在同一行上反复预留。
-- 历史列允许为空的不编造默认值（同 NEW-20 的约定）。
-- +goose Up
create table agent_session (
  id                     bigint primary key,
  workspace_id           bigint not null,
  project_id             bigint not null,
  payer_account_id       bigint not null,
  created_by_user_id     bigint not null,
  title                  varchar(100) not null default '',
  state                  varchar(24) not null default 'idle',
  skill_codes            jsonb not null default '[]',
  prefs                  jsonb not null default '{}',
  todo                   jsonb not null default '[]',
  summary                text,
  approved_budget        integer not null default 0,
  plan_doc_version       integer,
  runner_id              varchar(64),
  runner_lease_until     timestamptz,
  stop_requested_at      timestamptz,
  create_idempotency_key varchar(128),
  last_active_at         timestamptz not null default now(),
  created_at             timestamptz not null default now(),
  updated_at             timestamptz not null default now(),
  constraint ck_agent_session_state check (state in
    ('idle','planning','awaiting_input','awaiting_approval','executing','waiting_task','failed','cancelled')),
  constraint ck_agent_session_budget check (approved_budget >= 0)
);
create unique index uk_agent_session_idem on agent_session (workspace_id, create_idempotency_key)
  where create_idempotency_key is not null;
create index idx_agent_session_project on agent_session (project_id, last_active_at desc);
create index idx_agent_session_scan on agent_session (state, runner_lease_until)
  where state in ('planning','executing','waiting_task');

create table agent_turn (
  id               bigint primary key,
  session_id       bigint not null,
  trigger          varchar(16) not null,
  status           varchar(16) not null default 'pending',
  model_code       varchar(64),
  credits_cost     integer not null default 0,
  call_count       integer not null default 0,
  valid_outputs    integer not null default 0,
  idempotency_key  varchar(128),
  error_code       varchar(64),
  error_message    varchar(1024),
  started_at       timestamptz,
  finished_at      timestamptz,
  created_at       timestamptz not null default now(),
  constraint ck_agent_turn_trigger check (trigger in ('user','system')),
  constraint ck_agent_turn_status check (status in ('pending','running','success','failed','cancelled'))
);
create unique index uk_agent_turn_idem on agent_turn (session_id, idempotency_key)
  where idempotency_key is not null;
create index idx_agent_turn_session on agent_turn (session_id, created_at desc);
create index idx_agent_turn_running on agent_turn (status, started_at) where status = 'running';

create table agent_message (
  id            bigint primary key,
  session_id    bigint not null,
  turn_id       bigint,
  seq           integer not null,
  role          varchar(16) not null,
  content       text not null default '',
  refs          jsonb not null default '[]',
  tool_calls    jsonb,
  tool_call_id  varchar(64),
  card_id       bigint,
  created_at    timestamptz not null default now(),
  constraint ck_agent_message_role check (role in ('user','assistant','tool','system_note'))
);
create unique index uk_agent_message_seq on agent_message (session_id, seq);

create table agent_step (
  id             bigint primary key,
  session_id     bigint not null,
  turn_id        bigint not null,
  tool           varchar(48) not null,
  status         varchar(16) not null default 'running',
  attempt        integer not null default 1,
  args           jsonb not null default '{}',
  display        jsonb not null default '{}',
  generation_id  bigint,
  credits_quoted integer not null default 0,
  error_message  varchar(1024),
  started_at     timestamptz not null default now(),
  finished_at    timestamptz,
  constraint ck_agent_step_status check (status in ('running','success','failed','cancelled','waiting'))
);
create index idx_agent_step_turn on agent_step (turn_id, started_at);
create index idx_agent_step_generation on agent_step (generation_id) where generation_id is not null;
-- 扫描循环：waiting_task 的会话所等的步骤。
create index idx_agent_step_waiting on agent_step (session_id) where status = 'waiting';

create table agent_card (
  id            bigint primary key,
  session_id    bigint not null,
  step_id       bigint not null,
  kind          varchar(24) not null,
  payload       jsonb not null,
  status        varchar(16) not null default 'pending',
  submission    jsonb,
  submitted_by  bigint,
  submitted_at  timestamptz,
  created_at    timestamptz not null default now(),
  constraint ck_agent_card_kind check (kind in ('choice','questions','plan_confirm','budget','delete_confirm')),
  constraint ck_agent_card_status check (status in ('pending','submitted','expired','voided'))
);
create index idx_agent_card_pending on agent_card (session_id) where status = 'pending';
-- 过期扫描按创建时间取 pending 卡片。
create index idx_agent_card_expire on agent_card (created_at) where status = 'pending';

create table agent_budget_entry (
  id             bigint primary key,
  session_id     bigint not null,
  step_id        bigint not null,
  quoted         integer not null,
  status         varchar(16) not null,
  generation_id  bigint,
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now(),
  constraint ck_agent_budget_status check (status in ('reserved','charged','released','refunded')),
  constraint ck_agent_budget_quoted check (quoted >= 0)
);
create unique index uk_agent_budget_step on agent_budget_entry (step_id);
create index idx_agent_budget_session on agent_budget_entry (session_id, status);

-- +goose Down
drop table agent_budget_entry;
drop table agent_card;
drop table agent_step;
drop table agent_message;
drop table agent_turn;
drop table agent_session;
