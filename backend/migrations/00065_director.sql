-- 3D 导演台（docs/agent/director；DRT-01 建表，DRT-02 / 03 / 19 使用）。
-- 起因：
--   ① 场景（角色、机位、背景、时间轴）要单独存、带自己的乐观锁（DRK-9 / DRD-3）：放进 canvas_data 的话，
--      画布每次自动保存都要整份上传几十 KB；一个人挪节点、一个人调角色，就会在画布乐观锁上互相撞 409。
--   ② 截图 / 预演视频 / 泼溅文件要在服务端留记录（DRD-12）：删除保护、过期判定、项目清理都靠它；
--      只存在节点 params 里的话，删除保护要去扫没有索引的 canvas_data。
--   ③ AI 搭景免费，但要限流与追溯（DRD-23）：调用记录表，限流按库计数。
--   ④ media_object.purpose 新增三个值（DRD-15）：「我的素材」只列 upload_reference，新值天然不进（DRK-11）。
-- ⚠️ director_scene 没有 status 列：删除导演台节点不删场景（DR-F-06，撤销要能恢复），
--    场景只在项目清理时随项目物理删除（DRD-30）。加一个永远是 active 的列，只会诱使别人去写一条软删路径。
-- +goose Up
create table director_scene (
  id                  bigint primary key,
  workspace_id        bigint       not null,
  project_id          bigint       not null,
  canvas_node_id      varchar(64)  not null,
  scene               jsonb        not null,
  schema_version      integer      not null default 1,
  -- 乐观锁，与 schema_version 是两件事（CLAUDE.md §5.4）。
  version             integer      not null default 1,
  -- 服务端保存时计算（DRD-13）；过期 = 媒体记录上的指纹 ≠ 这里。
  render_fingerprint  varchar(32)  not null,
  -- 场景正文引用的媒体 ID（全景、泼溅），保存时提取（DRD-16）；字符串数组才能用 GIN 的 @>（同 MKD-31）。
  media_ids           text[]       not null default '{}',
  -- 最后一次写入的来源：网页 / Agent / MCP。冲突详情据此说「是谁改的」。
  source              varchar(16)  not null default 'user',
  last_editor_user_id bigint,
  agent_session_id    bigint,
  mcp_grant_id        bigint,
  created_at          timestamptz  not null default now(),
  updated_at          timestamptz  not null default now(),
  constraint ck_director_scene_source check (source in ('user','agent','mcp'))
);
-- 按 (项目, 画布节点) 幂等（DRD-17）：同一节点重复建场景返回已有那份。
create unique index uk_director_scene_node on director_scene (project_id, canvas_node_id);
create index idx_director_scene_ws_project on director_scene (workspace_id, project_id);
create index idx_director_scene_media on director_scene using gin (media_ids);

create table director_media (
  id                  bigint primary key,
  workspace_id        bigint       not null,
  project_id          bigint       not null,
  scene_id            bigint       not null,
  kind                varchar(16)  not null,
  media_id            bigint       not null,
  width               integer,
  height              integer,
  duration_ms         integer,
  fps                 integer,
  -- 机位快照：机位之后被改名、改焦距、被删掉，记录仍说得清「当时是哪一个、什么参数」。
  camera_id           varchar(64),
  camera_name         varchar(40),
  camera              jsonb,
  -- 截图时的场景版本与渲染指纹（泼溅为空）。
  scene_version       integer,
  render_fingerprint  varchar(32),
  is_hidden           boolean      not null default false,
  created_by_user_id  bigint,
  created_at          timestamptz  not null default now(),
  constraint ck_director_media_kind check (kind in ('capture','depth','pose','preview','splat'))
);
create index idx_director_media_scene on director_media (scene_id, created_at desc);
-- 删除保护按媒体反查（dev/01 §3.7）。
create index idx_director_media_media on director_media (media_id);
create index idx_director_media_project on director_media (project_id);

create table director_llm_call (
  id                  bigint primary key,
  workspace_id        bigint       not null,
  project_id          bigint       not null,
  scene_id            bigint       not null,
  user_id             bigint       not null,
  source              varchar(16)  not null,
  agent_session_id    bigint,
  mcp_grant_id        bigint,
  model_code          varchar(64)  not null,
  operation_code      varchar(32)  not null default 'scene_build',
  prompt_chars        integer      not null,
  image_count         integer      not null default 0,
  status              varchar(16)  not null default 'running',
  error_code          varchar(32),
  -- ⚠️ 写入前按字截断（pgtext.Fit，CD-82）：上游报错原文可能很长，超长会让整条记录写不进去，
  --    而那时调用早已结束 —— 记录停在 running，并发额度被永久占住。
  error_message       varchar(500),
  object_count        integer,
  duration_ms         integer,
  created_at          timestamptz  not null default now(),
  finished_at         timestamptz,
  constraint ck_director_llm_call_status check (status in ('running','succeeded','failed')),
  constraint ck_director_llm_call_source check (source in ('web','agent','mcp'))
);
-- 限流：按用户数「进行中」与「近 1 小时」（DRD-23）。
create index idx_director_llm_call_user on director_llm_call (user_id, created_at desc);
-- 恢复扫描只扫进行中的。
create index idx_director_llm_call_running on director_llm_call (created_at) where status = 'running';
create index idx_director_llm_call_project on director_llm_call (project_id);

alter table media_object drop constraint ck_media_purpose;
alter table media_object add constraint ck_media_purpose check (purpose in
    ('upload_reference','generation_output','render_output','script_asset',
     'banner','avatar','material_cover','export_archive',
     'director_capture','director_preview','director_asset'));

-- +goose Down
-- ⚠️ 库里已有导演台媒体时，恢复旧 CHECK 会失败。这是有意的：回退不该悄悄删用户的文件
--    （rollback.sh 默认不回退数据库，CLAUDE.md §5.27）。
alter table media_object drop constraint ck_media_purpose;
alter table media_object add constraint ck_media_purpose check (purpose in
    ('upload_reference','generation_output','render_output','script_asset',
     'banner','avatar','material_cover','export_archive'));
drop table director_llm_call;
drop table director_media;
drop table director_scene;
