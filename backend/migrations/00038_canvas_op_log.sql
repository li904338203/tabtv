-- 画布操作日志与 Agent 编辑者（AGT-10；AG-09、决策 D-2）
--
-- 起因：Agent 要在**画布没打开**的时候也能往画布上加节点、连线、画框，
-- 而画布此前只有「整份覆盖 + 乐观锁」一条写入路径（SaveCanvas）。
-- 让 Agent 也整份覆盖的话，打开着画布的用户每次都要整页重载，
-- 而且他手上没存的改动只能二选一地丢掉一边。
-- 现在服务端在**最新版本**上应用一批操作，并把「版本 N → N+1 是哪一批操作」
-- 记进 canvas_op_log：打开着的前端按批次重放（不重载、不闪），
-- 自己保存撞 40902 时也能补取中间缺的批次、重放后自动重存。
--
-- ⚠️ 只记**操作日志写入**的版本，不记真人的整份保存：前端补取时
-- 「区间内的版本数 ≠ 批次数」恰好就是「中间有真人改过」的判据（needsReload）。
--
-- ⚠️ 会无限增长：保留 30 天（设计建议值，PARAM-05 未定稿），Worker 每小时分批清理，
-- 清理按 created_at 删，所以建 idx_canvas_op_created。补取超过保留期的区间得到 needsReload。
--
-- `project.last_editor_agent_session_id`：最后一次写入是 Agent 时记会话 ID，
-- 真人整份保存时置空 —— 冲突详情据此分出「中间改动来自 Agent 还是真人」，
-- 前者前端可以无感重放，后者必须让用户选。
-- +goose Up
create table canvas_op_log (
  id                bigint primary key,
  project_id        bigint not null,
  from_version      integer not null,
  to_version        integer not null,
  batch_id          varchar(64) not null,
  actor_kind        varchar(16) not null,
  agent_session_id  bigint,
  user_id           bigint,
  ops               jsonb not null,
  created_at        timestamptz not null default now(),
  constraint ck_canvas_op_actor check (actor_kind in ('agent', 'user')),
  constraint ck_canvas_op_version check (to_version = from_version + 1)
);
create unique index uk_canvas_op_batch on canvas_op_log (project_id, batch_id);
create unique index uk_canvas_op_version on canvas_op_log (project_id, to_version);
create index idx_canvas_op_created on canvas_op_log (created_at);

alter table project add column last_editor_agent_session_id bigint;

-- +goose Down
alter table project drop column last_editor_agent_session_id;
drop table canvas_op_log;
