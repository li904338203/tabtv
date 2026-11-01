-- 智能剪辑（docs/agent/smartcut，SCT-02；DDL 依据 dev/02 §1.1）
--
-- 起因：
--   ① 画布新增「智能剪辑」节点，它背后是一条时间线（SCD-1 / 2）。时间线要能绑定画布节点、
--      记住最后是谁改的（冲突提示要分出 bySelf 与 Agent / MCP，SCD-17）、记住「剪过没有」
--      （连线即入轨只在没剪过时自动铺，SC-F-13 / 14）、记住素材指纹（「素材有变化」，SCD-9）。
--   ② 轨道要支持多条音频轨（配乐 / 人声 / 音效，人声闪避按用途找「谁压谁」）、字幕轨带语言、
--      新增文字轨（SCD-14 / 15）；片段要有画面变换、来源、来源节点、备选与进行中的生成（SCD-35）。
--   ③ 字幕要归属到字幕轨（原文 / 译文多轨，SC-F-52），并能跟随片段（SC-F-56）。
--      ⚠️ 存量字幕回填到各自时间线的默认字幕轨（SC-F-55）；`repo.SaveSubtitles` 同一次改动里写上 track_id，
--      否则迁移一跑完字幕保存就全部失败。
--   ④ 「撤销这轮 AI」要跨刷新，AI 写入之前存快照（SCD-18）；AI 剪辑免费但要记账与限流（SCD-24）。
--   ⑤ 预览版 / 拼图 / 波形 / 抽音轨 / 节拍 / 识别结果 / 看图描述按素材缓存（SCD-30 / 32）。
--      ⚠️ 唯一约束对排队与进行中的行也生效：只在 success 上唯一的话，两个并发请求会各建一行、各扣一次费。
--   ⑥ 导出要真正幂等（SCB-3）、冻结规格、记自检结果（SCD-29）；阶段补音频预渲 mixing 与自检 checking。
--   ⑦ 片段级生成要记下产物落到时间线的哪里（SCD-33）；派生媒体多三种（SCD-30）；
--      Provider 认证方式多一种火山 V4 签名（SCD-38）。
--   ⑧ 导出改为免费（SCK-3，2026-10-27 拍板）：timeline_render 的兜底价改为 0。运营仍可在后台改价。
--
-- ⚠️ 画面片段 start_ms 的语义从「不扣转场重叠的累计」改为成片时间（SCK-15）：画面轨仍由服务端算出、
--    只是缓存，下次 Reflow 时自然更新；音频轨的 start_ms 改由客户端放置（SCD-15）。不需要数据迁移。
-- +goose Up

-- ---------------------------------------------------------------- timeline
alter table timeline add column canvas_node_id      varchar(64);
-- 存量时间线（脚本装配、Agent 拼接）都当「剪过」：它们的内容是有意排出来的，不能被连线即入轨改动。
alter table timeline add column is_arranged         boolean not null default true;
alter table timeline add column last_editor_user_id bigint;
alter table timeline add column last_editor_kind    varchar(16);
alter table timeline add column planned_fingerprint varchar(64);
alter table timeline add column synced_fingerprint  varchar(64);
alter table timeline add constraint ck_timeline_editor_kind
    check (last_editor_kind is null or last_editor_kind in ('user','ai','agent','mcp','sync','system'));
-- 同一项目里一个节点只对应一条有效时间线（SCD-20）。
create unique index uk_timeline_node on timeline (project_id, canvas_node_id)
    where canvas_node_id is not null and status = 'editing';

-- ---------------------------------------------------------------- timeline_track
alter table timeline_track drop constraint ck_track_kind;
alter table timeline_track add constraint ck_track_kind
    check (kind in ('video','audio','subtitle','text'));
alter table timeline_track add column role      varchar(8);
alter table timeline_track add column lang      varchar(16);
alter table timeline_track add column is_hidden boolean not null default false;
alter table timeline_track add column style     jsonb;
alter table timeline_track add constraint ck_track_role
    check (role is null or role in ('music','voice','sfx'));

-- ---------------------------------------------------------------- timeline_clip
alter table timeline_clip add column transform             jsonb;
alter table timeline_clip add column origin                varchar(8) not null default 'user';
alter table timeline_clip add column source_node_id        varchar(64);
alter table timeline_clip add column alternates            jsonb not null default '[]'::jsonb;
alter table timeline_clip add column pending_generation_id bigint;
alter table timeline_clip add constraint ck_clip_origin check (origin in ('ai','user','sync'));

-- ---------------------------------------------------------------- timeline_subtitle
alter table timeline_subtitle add column track_id bigint;
alter table timeline_subtitle add column anchor   jsonb;
-- 没有字幕轨的时间线先补一条（新建时间线一律铺三条默认轨，正常不会缺；这里兜住历史脏数据）。
insert into timeline_track (id, timeline_id, kind, idx, name)
select (random() * 9e18)::bigint, t.id, 'subtitle', 1, '字幕'
from timeline t
where not exists (select 1 from timeline_track k where k.timeline_id = t.id and k.kind = 'subtitle');
update timeline_subtitle s set track_id = k.id
from timeline_track k
where k.timeline_id = s.timeline_id and k.kind = 'subtitle'
  and k.idx = (select min(k2.idx) from timeline_track k2
               where k2.timeline_id = s.timeline_id and k2.kind = 'subtitle');
alter table timeline_subtitle alter column track_id set not null;
create index idx_subtitle_track on timeline_subtitle (track_id, start_ms);

-- ---------------------------------------------------------------- 新表
-- 文字层（标题、贴字，SC-F-50）。起止为成片时间。
create table timeline_text (
    id          bigint primary key,
    timeline_id bigint      not null,
    track_id    bigint      not null,
    start_ms    integer     not null,
    end_ms      integer     not null,
    text        text        not null,
    style       jsonb       not null default '{}'::jsonb,
    box         jsonb       not null default '{}'::jsonb,
    created_at  timestamptz not null default now(),
    constraint ck_text_range check (end_ms > start_ms and start_ms >= 0)
);
create index idx_text_timeline on timeline_text (timeline_id, start_ms);

-- 快照（SCD-18）：内容与整份保存同形；每条时间线保留最近 20 份（应用层删旧）。
-- 快照里的媒体**不算引用**（SCK-18）：恢复时报缺失。
create table timeline_snapshot (
    id                 bigint primary key,
    timeline_id        bigint      not null,
    version            integer     not null,
    content            jsonb       not null,
    reason             varchar(16) not null,
    call_id            bigint,
    generation_id      bigint,
    created_by_user_id bigint,
    editor_kind        varchar(16),
    created_at         timestamptz not null default now(),
    constraint ck_snapshot_reason check (reason in
        ('ai_plan','ai_edit','ai_variant','clip_fill','replace_changed','subtitles','translate','restore'))
);
create index idx_snapshot_timeline on timeline_snapshot (timeline_id, created_at desc);

-- AI 剪辑 / 追加指令 / 翻译的调用记录（SCD-24）：免费，但要限流与追溯（同 director_llm_call）。
create table smartcut_llm_call (
    id                bigint primary key,
    workspace_id      bigint       not null,
    project_id        bigint       not null,
    timeline_id       bigint       not null,
    canvas_node_id    varchar(64),
    user_id           bigint       not null,
    source            varchar(8)   not null,
    kind              varchar(16)  not null,
    mode              varchar(16),
    instruction       text,
    spec              jsonb,
    variant_count     smallint     not null default 1,
    input_fingerprint varchar(64),
    model_code        varchar(64),
    stage             varchar(16)  not null default 'sync',
    status            varchar(16)  not null default 'running',
    base_version      integer,
    result            jsonb,
    applied_variant   smallint,
    used_fallback     boolean      not null default false,
    error_message     varchar(1024),
    duration_ms       integer,
    created_at        timestamptz  not null default now(),
    finished_at       timestamptz,
    constraint ck_sc_call_source check (source in ('web','agent','mcp')),
    constraint ck_sc_call_kind   check (kind in ('plan','edit','translate')),
    constraint ck_sc_call_mode   check (mode is null or mode in ('mix','board','talk','narr')),
    constraint ck_sc_call_stage  check (stage in ('sync','understand','plan','compile','apply','done')),
    constraint ck_sc_call_status check (status in ('running','success','failed'))
);
create index idx_sc_call_user on smartcut_llm_call (user_id, created_at desc);
create index idx_sc_call_timeline on smartcut_llm_call (timeline_id, created_at desc);
create index idx_sc_call_running on smartcut_llm_call (created_at) where status = 'running';

-- 按素材缓存的分析结果（SCD-30 / 32）：一行一个（媒体，种类，引擎，参数摘要）。
create table media_analysis (
    id               bigint primary key,
    workspace_id     bigint      not null,
    media_object_id  bigint      not null,
    kind             varchar(16) not null,
    engine           varchar(64) not null,
    params_key       varchar(64) not null default '',
    status           varchar(16) not null default 'queued',
    attempts         smallint    not null default 0,
    lease_until      timestamptz,
    result           jsonb,
    derived_media_id bigint,
    generation_id    bigint,
    error_message    varchar(512),
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    constraint ck_analysis_kind   check (kind in
        ('preview_proxy','filmstrip','peaks','speech_audio','beats','asr','caption')),
    constraint ck_analysis_status check (status in ('queued','running','success','failed'))
);
-- ⚠️ 排队与进行中也唯一：失败的行不占位，重试另插一行。
create unique index uk_media_analysis on media_analysis (media_object_id, kind, engine, params_key)
    where status <> 'failed';
create index idx_analysis_pending on media_analysis (created_at) where status in ('queued','running');

-- ---------------------------------------------------------------- render_job
alter table render_job add column idempotency_key varchar(100);
alter table render_job add column spec            jsonb;
-- ⚠️ check 是保留字，列名用 self_check。
alter table render_job add column self_check      jsonb;
create unique index uk_render_idem on render_job (workspace_id, idempotency_key)
    where idempotency_key is not null;
alter table render_job drop constraint ck_render_stage;
alter table render_job add constraint ck_render_stage
    check (stage is null or stage in ('probing','assembling','mixing','encoding','archiving','checking'));

-- ---------------------------------------------------------------- generation
-- 片段级生成的落点（SCD-33）：{timelineId, kind, clipId?, afterClipId?, atMs?, trackId?, mediaId?, paramsKey?, version}
alter table generation add column smartcut_target jsonb;
create index idx_generation_smartcut on generation ((smartcut_target->>'timelineId'))
    where smartcut_target is not null;

-- ---------------------------------------------------------------- media_object
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in
        ('grid_tile','share_import','video_poster','preview_proxy','filmstrip','speech_audio'));

-- ---------------------------------------------------------------- provider_config
alter table provider_config drop constraint ck_provider_auth;
alter table provider_config add constraint ck_provider_auth
    check (auth_type in ('none','bearer','header','volc_v4'));

-- ---------------------------------------------------------------- 导出免费（SCK-3）
update pricing set credits = 0, updated_at = now()
from operation o
where pricing.operation_id = o.id and o.code = 'timeline_render' and pricing.model_id is null;

-- +goose Down
update pricing set credits = 1, updated_at = now()
from operation o
where pricing.operation_id = o.id and o.code = 'timeline_render' and pricing.model_id is null;

update provider_config set auth_type = 'bearer', status = 'disabled' where auth_type = 'volc_v4';
alter table provider_config drop constraint ck_provider_auth;
alter table provider_config add constraint ck_provider_auth
    check (auth_type in ('none','bearer','header'));

update media_object set derive_kind = null
where derive_kind in ('preview_proxy','filmstrip','speech_audio');
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in ('grid_tile','share_import','video_poster'));

drop index if exists idx_generation_smartcut;
alter table generation drop column if exists smartcut_target;

update render_job set stage = null where stage in ('mixing','checking');
alter table render_job drop constraint ck_render_stage;
alter table render_job add constraint ck_render_stage
    check (stage is null or stage in ('probing','assembling','encoding','archiving'));
drop index if exists uk_render_idem;
alter table render_job drop column if exists self_check;
alter table render_job drop column if exists spec;
alter table render_job drop column if exists idempotency_key;

drop table if exists media_analysis;
drop table if exists smartcut_llm_call;
drop table if exists timeline_snapshot;
drop table if exists timeline_text;

drop index if exists idx_subtitle_track;
alter table timeline_subtitle drop column if exists anchor;
alter table timeline_subtitle drop column if exists track_id;

alter table timeline_clip drop constraint if exists ck_clip_origin;
alter table timeline_clip drop column if exists pending_generation_id;
alter table timeline_clip drop column if exists alternates;
alter table timeline_clip drop column if exists source_node_id;
alter table timeline_clip drop column if exists origin;
alter table timeline_clip drop column if exists transform;

delete from timeline_track where kind = 'text';
alter table timeline_track drop constraint if exists ck_track_role;
alter table timeline_track drop column if exists style;
alter table timeline_track drop column if exists is_hidden;
alter table timeline_track drop column if exists lang;
alter table timeline_track drop column if exists role;
alter table timeline_track drop constraint ck_track_kind;
alter table timeline_track add constraint ck_track_kind check (kind in ('video','audio','subtitle'));

drop index if exists uk_timeline_node;
alter table timeline drop constraint if exists ck_timeline_editor_kind;
alter table timeline drop column if exists synced_fingerprint;
alter table timeline drop column if exists planned_fingerprint;
alter table timeline drop column if exists last_editor_kind;
alter table timeline drop column if exists last_editor_user_id;
alter table timeline drop column if exists is_arranged;
alter table timeline drop column if exists canvas_node_id;
