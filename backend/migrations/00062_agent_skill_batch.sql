-- +goose Up
-- 起因（MKT-T-32；MKT-F-50~52、MKT-UX-08，MKT-T-14 / MKT-T-27 执行记录）：一次出多个变体的技能
-- （批量变体、素材裂变）在价格明细里要多一行「最多 N 个变体」，而技能此前没有任何字段说明它是不是批量 ——
-- 前端只能按两个技能 code 硬匹配（frontend/utils/skillEntries.ts 的 BATCH_SKILL_CODES）。运营换一个 code
-- 或新建一个批量技能，那一行提示就悄悄消失，而没有任何报错。
--
-- ⚠️ 这只是**展示与说明用的标识**，不是计费或执行的依据：变体上限仍是服务端的环境变量
--    （SEEAITV_MARKETING_MAX_VARIANTS，tasks/README §4 #1），批量执行按计划项逐项计费（MKT-T-27）。
--    把它当成「允许批量」的开关的话，一个漏勾的技能会让服务端拒绝合法的批量计划。
-- 现有技能取默认值 false；首批两个批量技能由运营在管理端勾上（生产 seed 不写营销数据，MKD-21）。
alter table agent_skill add column is_batch boolean not null default false;

-- +goose Down
alter table agent_skill drop column if exists is_batch;
