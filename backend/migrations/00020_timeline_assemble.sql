-- 时间线装配溯源与媒体时长（NEW-07、NEW-09、P10-01）
--
-- 起因一：「按分镜自动装配」之后要能回答**这一段是哪一镜**。
-- 不记的话，重新装配只能「全删重建」——而用户在装配之后往往已经手工
-- 裁过几段、调过顺序，全删重建等于把他的工作扔掉。
-- 记下来才能做到「只补缺的那几镜、不动已经改过的」。
--
-- 起因二：`media_object.duration_ms` 这一列从建表起就没被写过
-- （与 width/height 在 P09-02 之前的状况一样）。没有时长就没法校验
-- 「裁切区间不能超出素材本身」——而超出的后果是渲染出一段黑屏，
-- 且任务是"成功"的。
-- +goose Up

-- ⚠️ 逻辑外键、**不级联**：镜头删了，已经装进时间线的那一段仍然有效
-- （它是一份独立的素材引用）。source_shot_id 只是溯源线索，
-- 查不到镜头按"来源已删除"显示，不是错误。
alter table timeline_clip add column source_shot_id bigint;
create index idx_clip_shot on timeline_clip (source_shot_id) where source_shot_id is not null;

-- 自动装配的幂等与"只补缺口"都靠它：同一条时间线里，
-- 一镜最多只该有一段自动装配出来的片段。
-- ⚠️ 用户**手工复制**出来的第二段不受这条约束——它的 source_shot_id 为空，
-- 而 PG 的部分索引不覆盖 null 行。这是有意的：约束管的是自动装配，
-- 不是"用户不能把同一镜用两次"。
create unique index uk_clip_shot on timeline_clip (track_id, source_shot_id)
    where source_shot_id is not null;

-- +goose Down
drop index if exists uk_clip_shot;
drop index if exists idx_clip_shot;
alter table timeline_clip drop column if exists source_shot_id;
