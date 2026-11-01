-- +goose Up
-- 起因（产品决定：用户不再手动提交作品）：
--
--   此前作品要用户在「我的作品」里逐件点「提交审核」才进 work 表，绝大多数成功的
--   生成从没被提交过，管理端「作品审核」几乎是空的。现在改成**每次成功的生成
--   （图片/视频/音频，不含文本）自动登记为待审作品**（Worker 侧
--   generation/repo.RegisterWork），运营在「作品审核」里直接审。
--
--   本迁移把**历史上已成功**的媒体生成按同一口径补登：
--     - 媒体：按 idx 第一个有媒体的产出（与 API 的 workSourceGuard、Worker 的
--       RegisterWork 相同，不硬取 outputs[0] —— 一次生成可能先出文字再出图）；
--     - 标题：项目名去空白后截到 100 字，取不到给「未命名作品」；
--     - 一律 pending、submitted_at 为空、不加精不下架（公开仍要 approved ∧ featured ∧ ¬offline）；
--     - 已存在的 (source_type, source_id) 跳过（uk_work_source，**不碰已审过的**）。
--
-- ⚠️ 补登行的主键**取 generation.id 本身**（work.id = source_id）：两者出自同一个雪花
--   发号器，不会与任何已有 work.id 相撞；而运行时登记的作品主键是新发的号，必然
--   ≠ source_id。Down 段据此**只认本迁移补登的行**，用户手动提交的、Worker 自动登记的
--   都不会被误删；再叠加「仍是 pending、没被加精/下架、没有审核留痕」，人工审过的一律保留。
insert into work (id, workspace_id, source_type, source_id, media_object_id,
                  title, review_status, created_at, updated_at)
select g.id, g.workspace_id, 'generation', g.id, o.media_object_id,
       coalesce(nullif(left(btrim(p.name), 100), ''), '未命名作品'), 'pending',
       coalesce(g.finished_at, g.updated_at), now()
from generation g
join lateral (
    select x.media_object_id from generation_output x
    where x.generation_id = g.id and x.media_object_id is not null
      and x.output_type in ('image', 'video', 'audio')
    order by x.idx limit 1
) o on true
left join project p on p.id = g.project_id
where g.status = 'success' and g.result_type <> 'text'
on conflict do nothing;

-- +goose Down
delete from work w
where w.source_type = 'generation' and w.id = w.source_id
  and w.review_status = 'pending' and not w.is_featured and not w.is_offline
  and w.reviewed_by_admin_id is null
  and not exists (select 1 from content_review r where r.work_id = w.id);
