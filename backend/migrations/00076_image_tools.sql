-- 图片节点工具条：多角度 / 打光 / 情绪调节 / 高清与人物识别（docs/agent/imagetools，2026-10-30）。
-- 起因：
--   ① 工具条上「人像质感调节、全景、多角度、打光、高清」一直是「该功能尚未开放」的占位；
--      「高清」预设实际是 img2img + 一句「锐化」的提示词，scale 只进了提示词，像素一个都没变多（IMGB-2）。
--   ② 高清接 Topaz 精准放大，它**按输出像素收费**：计价要能按输出百万像素算（per_mpx），
--      否则 6 倍与 2 倍同价；min_units 兜住「放大一张小图只收 0 积分」。
--   ③ 情绪调节要先识别图里的人物：结果按图片缓存进 media_analysis（kind = people），同一张图不重复识别。
--   ④ 高清产物动辄几十 MP：归档时另存一张 1024 宽的预览（derive_kind = preview_image），
--      列表与节点卡取它，不去解整张大图（IMGB-1）。
--   操作码（multi_angle / relight / expression_edit / upscale / person_detect）在 seed 里，不在这里。
-- +goose Up
alter table pricing drop constraint ck_pricing_unit;
alter table pricing add constraint ck_pricing_unit
    check (unit in ('per_time','per_image','per_second','per_kchar','per_mpx'));
alter table pricing add column min_units integer not null default 1;
alter table pricing add constraint ck_pricing_min_units check (min_units >= 1);

alter table media_analysis drop constraint ck_analysis_kind;
alter table media_analysis add constraint ck_analysis_kind check (kind in
    ('preview_proxy','filmstrip','peaks','speech_audio','beats','asr','caption','people'));

alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in
        ('grid_tile','share_import','video_poster','video_lastframe','preview_proxy','filmstrip','speech_audio',
         'preview_image'));

-- +goose Down
update media_object set derive_kind = null where derive_kind = 'preview_image';
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in
        ('grid_tile','share_import','video_poster','video_lastframe','preview_proxy','filmstrip','speech_audio'));

delete from media_analysis where kind = 'people';
alter table media_analysis drop constraint ck_analysis_kind;
alter table media_analysis add constraint ck_analysis_kind check (kind in
    ('preview_proxy','filmstrip','peaks','speech_audio','beats','asr','caption'));

delete from pricing where unit = 'per_mpx';
alter table pricing drop constraint ck_pricing_min_units;
alter table pricing drop column min_units;
alter table pricing drop constraint ck_pricing_unit;
alter table pricing add constraint ck_pricing_unit
    check (unit in ('per_time','per_image','per_second','per_kchar'));
