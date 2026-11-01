-- 视频的封面帧，以及项目封面把视频算进来（CD-83）
--
-- 起因：用户反馈 Agent 项目出了视频，卡片上却一直是占位图。封面由 `project_display_cover()`（00030/00031）
-- 算，第二候选只认「最近一张成功产出的**图片**」—— 视频与成片根本不在候选里。查了本地库：
-- 最近 5 个 Agent 项目的展示封面全是 0，其中两个已经有成功的视频。
--
-- ── 为什么是「封面帧」而不是直接拿视频当封面 ──
-- 列表里每张卡片都把整段视频取下来放进 <video>，一屏几十个项目就是几百 MB 的内存（与 00030 时
-- 作品墙原图撑爆标签页是同一个问题）；而媒体端点的缩略图本来就只对位图生效、视频原样返回。
-- 所以给每个视频派生一张**静态的封面帧**（`derive_kind = 'video_poster'`），封面走的仍是图片 ——
-- 前端不用改，缩略图（?w=320）照常可用。
--
-- ── 为什么是最后一帧 ──
-- AI 视频的第一帧多半就是喂进去的那张首帧图，最后一帧才是「这段视频变成了什么」。
-- ⚠️ 但成片常以淡出到黑场结尾：最后一帧是黑的就往前找（80% / 60% / 40% 处），全黑才如实用最后一帧。
-- 截帧在 Worker 的扫描里做（`media/app.PosterScanner`），不进出片的主路径：封面是锦上添花，
-- 截帧失败不能让一段已经扣过费、已经归档的视频变成「失败」。
--
-- ── 封面规则的变化 ──
-- 第二候选从「最近一张产出的图片」改成「**最近一次产出**」：图片用图本身，视频与成片用它的封面帧；
-- 封面帧还没截出来的那一项先跳过（退到下一候选），截出来之后自然生效。
-- 其余不变：显式封面优先、继承封面兜底、图被删了退回下一候选。
--
-- ⚠️ 三参数函数是**原地替换**（create or replace），两参数的旧签名仍转调它（00031 保留的那个），
--    所以回退窗口里旧二进制照常能列项目。
-- +goose Up

alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in ('grid_tile', 'share_import', 'video_poster'));

-- 截帧扫描捞「还没有封面帧的视频」：只看活着的、本身不是派生物的视频，按 id 倒序（新的先截）。
create index idx_media_video on media_object (id desc)
    where status = 'active' and source_media_id is null and content_type like 'video/%';

-- 项目封面的视频候选按项目取（同 00030 给图片建的那条）。
create index idx_generation_project_video on generation (project_id, created_at desc, id desc)
    where status = 'success' and result_type = 'video';

-- +goose StatementBegin
create or replace function project_display_cover(
    p_project_id bigint, p_explicit bigint, p_inherited bigint)
returns bigint
language sql
stable
as $$
  select coalesce(
    -- ① 用户显式设置的封面（且那张图还在）
    (select m.id from media_object m where m.id = p_explicit and m.status = 'active'),
    -- ② 项目自己最近一次成功的产出：图片用图本身，视频与成片用封面帧
    (select c.media_id from (
        select o.media_object_id as media_id, g.created_at as at, g.id as ref_id, o.idx
          from generation g
          join generation_output o
            on o.generation_id = g.id and o.output_type = 'image'
          join media_object m
            on m.id = o.media_object_id and m.status = 'active'
         where g.project_id = p_project_id
           and g.status = 'success'
           and g.result_type = 'image'
        union all
        -- ⚠️ 视频本身也得还在：用户删了那段视频，就不该再拿它的帧当封面
        select pm.id, g.created_at, g.id, o.idx
          from generation g
          join generation_output o
            on o.generation_id = g.id and o.output_type = 'video'
          join media_object v
            on v.id = o.media_object_id and v.status = 'active'
          join media_object pm
            on pm.source_media_id = v.id and pm.derive_kind = 'video_poster' and pm.status = 'active'
         where g.project_id = p_project_id
           and g.status = 'success'
           and g.result_type = 'video'
        union all
        select pm.id, r.created_at, r.id, 0
          from timeline t
          join render_job r
            on r.timeline_id = t.id and r.status = 'success'
          join media_object v
            on v.id = r.output_media_id and v.status = 'active'
          join media_object pm
            on pm.source_media_id = v.id and pm.derive_kind = 'video_poster' and pm.status = 'active'
         where t.project_id = p_project_id
      ) c
      order by c.at desc, c.ref_id desc, c.idx
      limit 1),
    -- ③ 复制时从源项目继承的封面（且那张图还在）
    (select m.id from media_object m where m.id = p_inherited and m.status = 'active'),
    0)
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
create or replace function project_display_cover(
    p_project_id bigint, p_explicit bigint, p_inherited bigint)
returns bigint
language sql
stable
as $$
  select coalesce(
    (select m.id from media_object m where m.id = p_explicit and m.status = 'active'),
    (select o.media_object_id
       from generation g
       join generation_output o
         on o.generation_id = g.id and o.output_type = 'image'
       join media_object m
         on m.id = o.media_object_id and m.status = 'active'
      where g.project_id = p_project_id
        and g.status = 'success'
        and g.result_type = 'image'
      order by g.created_at desc, g.id desc, o.idx
      limit 1),
    (select m.id from media_object m where m.id = p_inherited and m.status = 'active'),
    0)
$$;
-- +goose StatementEnd

drop index if exists idx_generation_project_video;
drop index if exists idx_media_video;
-- 回退时封面帧的行删掉（对象存储里的文件留下，与回退其余派生物同一个处理）；不删的话约束加不回去。
delete from media_object where derive_kind = 'video_poster';
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in ('grid_tile', 'share_import'));
