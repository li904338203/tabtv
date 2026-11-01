-- 智能剪辑（docs/agent/smartcut SCT-10）：时间线片段按素材查的两个索引。
-- 起因：
--   ① 删除保护（media/app 的引用检查器）每删一份素材都要问「有没有时间线片段在用」。timeline_clip 上一直没有
--      media_object_id 的索引，这条查询从来都是全表扫描 —— 片段少时看不出来，表越大删除越慢。
--   ② 智能剪辑的片段还有「备选」（alternates，重拍 / 去字幕之前的原片，可以切回）：它们同样要挡住删除，
--      按 `alternates @> '[{"mediaId":"…"}]'` 查，没有 GIN 索引也是全表扫描。
-- ⚠️ 只加索引，不改数据；表很小的本机与 CI 上建索引是瞬间的事。
-- +goose Up
create index idx_clip_media on timeline_clip (media_object_id);
create index idx_clip_alternates on timeline_clip using gin (alternates jsonb_path_ops);

-- +goose Down
drop index if exists idx_clip_alternates;
drop index if exists idx_clip_media;
