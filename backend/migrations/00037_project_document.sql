-- 项目文档：画布上的「文档节点」背后那份 Markdown 正文（AGT-09，AG-10）
--
-- 起因：Agent 的规划（/plan.md）、分镜表（/shots.md）、项目记忆（/.memory.md）
-- 以文档形式存在，并作为画布上的文档节点出现。正文**不进 canvas_data** ——
-- 画布每次自动保存都整份上传 canvasData，把几十 KB 的正文塞进节点的话，
-- 改一个节点位置就要把所有文档重传一遍；而且两个人一个改文档、一个挪节点，
-- 会在画布的乐观锁上互相撞 409，明明改的不是同一个东西。
-- 所以节点上只存 `params.docId`，正文在这张表里、各自带自己的 version 乐观锁。
--
-- 归属 canvas 域（拥有 project 的那个域）：它在阶段 A 落地，那时 agent 域还不存在；
-- 阶段 B 的 agent 经 Documents 端口读写，不直接查这张表。
--
-- ⚠️ path 唯一只约束 active 行（部分唯一索引）：软删的文档不该挡住同名新建，
--    否则「删了 /plan.md 再让 Agent 重新规划」会撞唯一索引、报一句对不上号的「名称已存在」。
-- ⚠️ workspace_id 冗余存一份：每条查询都带 workspace_id 条件（CD-04 第 2 条），
--    只靠 project_id 的话，要么每次 join project，要么就少了那道越权闸。
-- ⚠️ 逻辑外键（不建 FK）：与 project 的其余从属表同一约定；项目彻底删除时文档行
--    目前不随之删除（见 AGT-09 执行记录的挂账）。
-- +goose Up
create table project_document (
  id                  bigint primary key,
  workspace_id        bigint not null,
  project_id          bigint not null,
  path                varchar(200) not null,
  content             text not null default '',
  version             integer not null default 1,
  source              varchar(16) not null,
  last_editor_user_id bigint,
  agent_session_id    bigint,
  status              varchar(16) not null default 'active',
  created_at          timestamptz not null default now(),
  updated_at          timestamptz not null default now(),
  constraint ck_project_document_source check (source in ('agent', 'user')),
  constraint ck_project_document_status check (status in ('active', 'deleted'))
);
create unique index uk_project_document_path on project_document (project_id, path)
  where status = 'active';
create index idx_project_document_project on project_document (workspace_id, project_id);

-- +goose Down
drop table if exists project_document;
