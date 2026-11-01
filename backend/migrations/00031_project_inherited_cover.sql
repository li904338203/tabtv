-- 副本继承源项目的封面（P15-02）
--
-- 起因：00030 之后，出过图的项目会自动拿最近一张产出图当封面 —— 但**复制出来的副本
-- 永远是占位图**。展示封面只看项目**自己的**生成记录，而副本按设计一条都不拥有：
--   · CV-03：复制不带 generation 行与账本流水（副本不该凭空拥有一份消费记录）；
--   · GAP-16：节点结果里的 `id` 被改写成 `sourceGenerationId`（副本能预览，但不能把
--     原项目的产物当成自己的输入）。
-- 于是用户复制一个有封面的项目，得到一个画布里明明有图、卡片上却是占位的副本。
--
-- ── 为什么是「继承封面」这一列，而不是把封面写进 cover_media_id ──
-- `cover_media_id` 的含义是「用户显式选的封面」（00030 专门守住了这一条）。
-- 把源项目自动挑的图写进副本的这一列，会有三个后果，都不报错：
--   1. 副本从此**被钉死**在这张图上 —— 显式封面优先，副本自己出了新图也不换；
--   2. 删除保护把显式封面当作引用（`canvas/repo/references.go`），于是用户在素材库里
--      **删不掉那张图**，理由是「正被项目封面引用」—— 而他从没把它设成过封面；
--   3. 「用户选的」与「系统挑的」再也分不开。
-- 继承封面**只在复制时写一次**（唯一会让项目拥有「不是自己产出的图」的路径），
-- 优先级排在「自己的产出图」之后：副本一开始和源项目长得一样，自己出了新图就换成新图。
-- 它**不算引用**：那张图被删掉时，封面退回下一候选，而不是拦着不让删。
--
-- ⚠️ 为什么保留两参数的旧函数：迁移跑完到新二进制起来之间（以及**版本回退**时），
--    旧二进制仍在调 `project_display_cover(id, cover_media_id)`。直接删掉的话，那段时间里
--    **每一个项目列表都 500**，而 CD-69 的约定恰恰是「库超前于二进制必须放行」——
--    否则回退本身就做不了。旧签名改成转调新签名，下一次不再需要回退到 00030 之前时再删。
-- +goose Up

alter table project add column inherited_cover_media_id bigint;

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
    -- ② 项目自己最近一张成功产出的图片（规则同 00030）
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
    -- ③ 复制时从源项目继承的封面（且那张图还在）—— 排在自己的产出之后，
    --    副本自己出了新图就换成新图，不会被钉死在源项目的图上
    (select m.id from media_object m where m.id = p_inherited and m.status = 'active'),
    0)
$$;
-- +goose StatementEnd

-- 旧签名转调新签名（见文件头「为什么保留两参数的旧函数」）。
-- +goose StatementBegin
create or replace function project_display_cover(p_project_id bigint, p_explicit bigint)
returns bigint
language sql
stable
as $$
  select project_display_cover(p_project_id, p_explicit, null)
$$;
-- +goose StatementEnd

-- ── 回填：本迁移之前复制出来的副本 ──────────────────────────────
-- 副本的画布里，节点结果带着 CopyCanvasData 写下的 `sourceGenerationId`（只有复制会写这个键），
-- 这是唯一可靠的来源线索。取其中最近一次成功的图片生成的产物。
-- ⚠️ 画布 JSON 是客户端写的：
--    · ID 用 CASE 守住再转 bigint —— 一个非数字的值会让整条迁移失败；
--    · **限定同一工作区** —— 否则往自己画布里写一个别人的生成 ID，
--      就能让自己的项目封面指向别的租户的图。
update project p
   set inherited_cover_media_id = src.media_id
  from (
    select p2.id as project_id,
           (select o.media_object_id
              from jsonb_array_elements(
                     case when jsonb_typeof(p2.canvas_data -> 'nodes') = 'array'
                          then p2.canvas_data -> 'nodes' else '[]'::jsonb end) n
              join generation g
                on g.id = case when (n -> 'result' ->> 'sourceGenerationId') ~ '^[0-9]{1,18}$'
                               then (n -> 'result' ->> 'sourceGenerationId')::bigint end
               and g.workspace_id = p2.workspace_id
               and g.status = 'success'
               and g.result_type = 'image'
              join generation_output o
                on o.generation_id = g.id and o.output_type = 'image'
              join media_object m
                on m.id = o.media_object_id and m.status = 'active'
             order by g.created_at desc, g.id desc, o.idx
             limit 1) as media_id
      from project p2
     where p2.inherited_cover_media_id is null
  ) src
 where p.id = src.project_id
   and src.media_id is not null;

-- +goose Down

-- 两参数版本还原成 00030 的独立实现，再删三参数版本与列。
-- +goose StatementBegin
create or replace function project_display_cover(p_project_id bigint, p_explicit bigint)
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
    0)
$$;
-- +goose StatementEnd
drop function if exists project_display_cover(bigint, bigint, bigint);
alter table project drop column if exists inherited_cover_media_id;
