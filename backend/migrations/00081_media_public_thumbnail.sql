-- 公开缩略图存成派生媒体（2026-11-02，对象存储迁到火山 TOS 之后）。
-- 起因：公开封面（营销选项、技能、精选作品）原先由 API 每次取原图（3~4MB）→ 现场缩略 → 回字节，
--   测试服在海外、TOS 在北京，一张 320 宽的封面首字节 2 秒多。现在缩略图生成一次存起来
--   （derive_kind='public_thumb'，derive_key='w320'），之后 API 只判可见性、302 到对象存储的短时效签名地址。
--   单独一个用途 public_thumbnail：不混进任何按用途列的列表（素材库只列 upload_reference、生成历史只列产物）。
-- +goose Up
alter table media_object drop constraint ck_media_purpose;
alter table media_object add constraint ck_media_purpose check (purpose in
    ('upload_reference','generation_output','render_output','script_asset',
     'banner','avatar','material_cover','export_archive',
     'director_capture','director_preview','director_asset','public_thumbnail'));
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in
        ('grid_tile','share_import','video_poster','video_lastframe','preview_proxy','filmstrip','speech_audio',
         'public_thumb'));

-- +goose Down
-- ⚠️ 库里已有公开缩略图时，恢复旧 CHECK 会失败：先清掉它们（它们随时可以重新生成）。
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in
        ('grid_tile','share_import','video_poster','video_lastframe','preview_proxy','filmstrip','speech_audio'));
alter table media_object drop constraint ck_media_purpose;
alter table media_object add constraint ck_media_purpose check (purpose in
    ('upload_reference','generation_output','render_output','script_asset',
     'banner','avatar','material_cover','export_archive',
     'director_capture','director_preview','director_asset'));
