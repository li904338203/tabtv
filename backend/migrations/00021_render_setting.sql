-- 渲染水印配置与成片入库（NEW-11、GAP-13、P10-04）
--
-- 起因一：水印是"AI 生成内容"标识。旧系统有 `user.is_ai_watermark_off`
-- 这个开关，但**没有任何地方统一应用它**（GAP-13 的原文）——
-- 部分协议带 watermark 参数，成片这条链上一个字都没有。
--
-- 起因二：**水印图是配置不是代码**。换 logo、调位置、调透明度都不该发版
-- （与图片预设、提示词模板同一条）。所以单开一张单行配置表，
-- 跟 `auth_setting` 同一个形态。
--
-- 起因三：渲染任务要记**这次到底加没加水印**。不记的话，事后
-- 「这个片子怎么没水印」只能靠猜 —— 是用户关了？是那会儿没配？
-- 还是渲染漏了？三种的处理完全不同。
-- +goose Up

create table render_setting (
    id                  bigint      primary key,
    -- 水印图（media_object）。为空＝不加水印。
    --
    -- ⚠️ 逻辑外键：图被删掉时渲染**不加水印而不是失败** ——
    -- 为一张 logo 让整条渲染链停掉不值，但要在任务上记下没加。
    watermark_media_id  bigint,
    watermark_position  varchar(16) not null default 'bottom_right',
    -- 不透明度与缩放都按**百分比**：像素值换画幅就不对了
    -- （1080p 上合适的 120px 到竖屏上就压住半个画面）。
    watermark_opacity   integer     not null default 60,
    watermark_scale     integer     not null default 12,
    version             integer     not null default 1,
    updated_by_admin_id bigint,
    created_at          timestamptz not null default now(),
    updated_at          timestamptz not null default now(),
    constraint ck_wm_position check (watermark_position in
        ('top_left','top_right','bottom_left','bottom_right')),
    constraint ck_wm_opacity  check (watermark_opacity between 1 and 100),
    constraint ck_wm_scale    check (watermark_scale between 1 and 50)
);

-- ⚠️ 这次渲染**到底加没加**水印，与"配置里开没开"是两件事：
-- 用户可能自己关了（is_ai_watermark_off），也可能那会儿平台还没配图。
-- 只看配置的话，事后对不上账。
alter table render_job add column watermark_applied boolean not null default false;

-- +goose Down
alter table render_job drop column if exists watermark_applied;
drop table if exists render_setting;
