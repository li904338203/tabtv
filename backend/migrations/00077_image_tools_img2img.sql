-- 图片节点工具条改用现有图片大模型走图生图（docs/agent/imagetools dev/01 R-5，2026-10-31 用户决定）。
-- 起因：用户决定**不再接 fal.ai 与 Topaz**，多角度 / 打光 / 情绪调节 / 高清一律用现有图片大模型（图生图），
--   提示词由服务端拼；情绪调节改为手动框选（不做自动人物识别）。00076 里只为它们加的东西在这里撤掉，不留残余：
--   ① 计价单位 per_mpx 与 pricing.min_units（只为 Topaz 按输出像素计费而加）；
--   ② media_analysis 的 people 种类（自动人物识别的缓存）、media_object 的 preview_image 派生种类（从未使用）；
--   ③ 操作码 multi_angle / relight / expression_edit / upscale / person_detect —— 工具子节点的操作一律是 img2img，
--      这几个操作码连同它们的绑定、能力矩阵、价格一起删（仍被历史任务引用的不删，避免任务指向不存在的操作）；
--   ④ 启动时同步进 api_protocol 的 fal_queue / fal_detect / topaz_image 三个协议行（没有模型在用时删）。
-- +goose Up
delete from pricing where unit = 'per_mpx';
alter table pricing drop constraint ck_pricing_min_units;
alter table pricing drop column min_units;
alter table pricing drop constraint ck_pricing_unit;
alter table pricing add constraint ck_pricing_unit
    check (unit in ('per_time','per_image','per_second','per_kchar'));

delete from media_analysis where kind = 'people';
alter table media_analysis drop constraint ck_analysis_kind;
alter table media_analysis add constraint ck_analysis_kind check (kind in
    ('preview_proxy','filmstrip','peaks','speech_audio','beats','asr','caption'));

update media_object set derive_kind = null where derive_kind = 'preview_image';
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in
        ('grid_tile','share_import','video_poster','video_lastframe','preview_proxy','filmstrip','speech_audio'));

delete from pricing where operation_id in (select id from operation
    where code in ('multi_angle','relight','expression_edit','upscale','person_detect'));
delete from model_input_capability where operation_id in (select id from operation
    where code in ('multi_angle','relight','expression_edit','upscale','person_detect'));
delete from model_operation where operation_id in (select id from operation
    where code in ('multi_angle','relight','expression_edit','upscale','person_detect'));
delete from operation o
where o.code in ('multi_angle','relight','expression_edit','upscale','person_detect')
  and not exists (select 1 from generation g where g.operation_id = o.id);

delete from api_protocol a
where a.code in ('fal_queue','fal_detect','topaz_image')
  and not exists (select 1 from model m where m.api_protocol = a.code);

-- +goose Down
-- 只恢复约束与列（被删的操作码与协议行不回填：它们的实现已从代码里删除）。
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in
        ('grid_tile','share_import','video_poster','video_lastframe','preview_proxy','filmstrip','speech_audio',
         'preview_image'));

alter table media_analysis drop constraint ck_analysis_kind;
alter table media_analysis add constraint ck_analysis_kind check (kind in
    ('preview_proxy','filmstrip','peaks','speech_audio','beats','asr','caption','people'));

alter table pricing drop constraint ck_pricing_unit;
alter table pricing add constraint ck_pricing_unit
    check (unit in ('per_time','per_image','per_second','per_kchar','per_mpx'));
alter table pricing add column min_units integer not null default 1;
alter table pricing add constraint ck_pricing_min_units check (min_units >= 1);
