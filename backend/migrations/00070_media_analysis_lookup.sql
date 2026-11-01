-- 智能剪辑（docs/agent/smartcut SCT-16）：分析缓存 media_analysis 按素材查的两个索引。
-- 起因：
--   ① 剪辑台取预览素材（GET /media/{id}/preview-assets）按 media_object_id 列出这份素材的全部分析行（含失败的，
--      要告诉前端「失败了」）。唯一索引 uk_media_analysis 是部分索引（status <> 'failed'），查全部状态时用不上。
--   ② 回收器永久删除一份媒体时，要连带删掉以它为源、或以它为产物（预览版 / 抽出的音轨）的分析行 —— 不删的话
--      分析行还说「成功」、指着一份已经不存在的预览版，剪辑台永远取不到，也永远不会重做。
--      derived_media_id 没有索引的话，每永久删一份媒体就扫一遍这张表。
-- ⚠️ 只加索引，不改数据。
-- +goose Up
create index idx_analysis_media on media_analysis (media_object_id);
create index idx_analysis_derived on media_analysis (derived_media_id) where derived_media_id is not null;

-- +goose Down
drop index if exists idx_analysis_derived;
drop index if exists idx_analysis_media;
