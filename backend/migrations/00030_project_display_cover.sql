-- 项目的展示封面：显式封面优先，没有就取最近一张成功产出的图片（P15-02）
--
-- 起因：工作台 / AI 视频页的「最近项目」与「全部项目」一律是占位图 ——
-- `project.cover_media_id` 只有用户手动「设为封面」时才会写，而**没有任何人会去设**；
-- 库里一个出过图的项目，它的封面仍然是 null。用户期望的是：项目里有产出图片，
-- 就用其中一张当封面。
--
-- ── 为什么算出来，不写进去 ──────────────────────────────────
-- 另一种做法是「生成成功时顺手把 cover_media_id 写上」。那条路有三个问题：
--   1. **登记式**：要求每一条会产出图片的路径（节点生成、组子项、脚本资产、
--      图片工具、整图运行、受控重试……）都记得写一次，漏掉任何一条都不会有
--      编译错误 —— 与 CD-36（提示词 ready/stale）、CD-68（媒体引用）是同一类；
--   2. **跨域写**：生成域去改画布域的 `project` 行（而那一行还带着画布乐观锁）；
--   3. **分不清是谁设的**：自动写进去之后，「用户选的封面」与「系统挑的封面」
--      是同一个值 —— 用户删了那张图，或者后来又出了更好的图，都没法区分对待。
-- 算出来的版本三条都不存在，且**删掉那张图之后自动换下一张**，不会留下一张裂图。
--
-- ── 为什么是数据库函数 ──────────────────────────────────────
-- 读项目封面的有两个域：canvas（`projectCols`，7 处复用）与 content
-- （首页「最近项目」，按 CD-61 的决定直接查 `project`）。规则只能有**一个**定义 ——
-- 两边各写一段 SQL 的话，早晚一边改了另一边没改，而表现只是「首页和全部项目页
-- 的封面不一样」，没有任何报错。Go 常量又没法跨域共享（那等于在 `platform`
-- 里放业务规则）。这是本项目**第一个** SQL 函数，理由就是上面这一条。
--
-- ⚠️ **不覆盖 `cover_media_id` 的含义**：它仍然只表示「用户显式设置的封面」。
--    画布域有两处会把它读回去写 —— 改名/改封面时登记媒体引用、复制项目时
--    原样带给副本。若把它换成「展示封面」，改一次名就会给一张用户从没选过的图
--    登记引用，复制出来的副本会拿源项目的产出图当显式封面（从此不再跟着自己
--    的产出变，源项目那张图也删不掉）—— 两处都不报错。
--    所以展示封面是**另一个字段**（API 上是 `displayCoverMediaId`）。
-- +goose Up

-- +goose StatementBegin
create or replace function project_display_cover(p_project_id bigint, p_explicit bigint)
returns bigint
language sql
stable
as $$
  select coalesce(
    -- 显式封面优先 —— 但只在那张图还在的时候。被删掉的显式封面**退回自动挑选**，
    -- 而不是给出一个取不到的 ID（那在列表上就是一张裂图）。
    (select m.id from media_object m where m.id = p_explicit and m.status = 'active'),
    -- 最近一张成功产出的图片。只认 image：视频不能直接当 <img> 的封面
    -- （要另外抽帧），文本/音频没有画面。
    (select o.media_object_id
       from generation g
       join generation_output o
         on o.generation_id = g.id and o.output_type = 'image'
       join media_object m
         on m.id = o.media_object_id and m.status = 'active'
      where g.project_id = p_project_id
        and g.status = 'success'
        and g.result_type = 'image'
      -- 同一次生成出多张时取第 1 张；多次生成取最近一次 —— 封面反映项目的**当前**状态。
      order by g.created_at desc, g.id desc, o.idx
      limit 1),
    0)
$$;
-- +goose StatementEnd

-- ⚠️ 列表一页 12~20 个项目，每个项目都要跑一次上面的子查询。已有的
--    `idx_generation_node (project_id, node_id, created_at desc)` 第二列是 node_id，
--    **按项目取最新一条用不上它的排序** —— 项目里生成一多就退化成「扫这个项目的
--    全部生成再排序」。代价随生成数线性上升，而低数据量下完全看不出来（CD-67 同一类）。
--    部分索引只收成功的图片生成，正好是这条子查询要的那一小撮。
create index idx_generation_project_image
    on generation (project_id, created_at desc, id desc)
    where status = 'success' and result_type = 'image';

-- +goose Down
drop index if exists idx_generation_project_image;
drop function if exists project_display_cover(bigint, bigint);
