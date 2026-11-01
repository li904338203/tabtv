-- 视频封面改取第一帧，末帧另存一种派生（2026-10-30 用户需求）。
-- 起因：
--   ① 封面帧（video_poster，CD-83）原先取**最后一帧**。资产库、节点卡、项目封面上用户认一段视频靠的是
--      「它从哪儿开始」，结尾常是转场中途或淡出，认不出来 —— 用户要求封面用第一帧。
--   ② 但智能剪辑「补镜」（cliptools.fillReference）一直拿封面帧当「上一段的末帧」作参考图：直接把封面换成第一帧的话，
--      补出来的镜头会从上一段的开头重新开始，而任务照样成功、没有任何报错。所以末帧另立一种派生 video_lastframe。
--   ③ 库里现有的 video_poster 截的**就是末帧**：原样改成 video_lastframe（补镜不中断），
--      封面由 Worker 的 PosterScanner 按新规则（第一帧）重新补截 —— 补齐之前视频暂时没有封面帧，
--      项目封面按 project_display_cover() 的其余候选显示，不会报错。
-- +goose Up
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in
        ('grid_tile','share_import','video_poster','video_lastframe','preview_proxy','filmstrip','speech_audio'));

update media_object set derive_kind = 'video_lastframe' where derive_kind = 'video_poster';

-- +goose Down
-- 回到「封面 = 末帧」：丢掉新截的第一帧封面（按新规则补出来的），末帧改回 video_poster。
delete from media_object where derive_kind = 'video_poster';
update media_object set derive_kind = 'video_poster' where derive_kind = 'video_lastframe';

alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in
        ('grid_tile','share_import','video_poster','preview_proxy','filmstrip','speech_audio'));
