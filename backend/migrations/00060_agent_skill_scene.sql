-- +goose Up
-- 起因（MKT-T-05 / MKT-D-10~12）：营销 Agent 的技能要按场景归属、声明输入槽、带预填描述，
-- 而这三样此前都不存在 —— 营销页只能把全部技能原样列出来，创造页也会冒出只适合营销的技能。
--
-- ⚠️ scenes 为空 = 只在通用场景出现（「创造」页）；营销页只列含 'marketing' 的；
--    同时属于两边写 ["general","marketing"]。取值由应用层校验（agent/domain.ValidateSkill），
--    不加 CHECK：jsonb 数组的成员约束写成 CHECK 读不懂，且一改取值就要动表。
-- ⚠️ input_slots 的 key 只认 product / character / source_video（格式契约，运营不能新造），
--    形状由 marketing/domain.ValidateSlotDecl 经 agent 的 SlotDeclValidator 端口校验。
-- ⚠️ prefill_text 按码点 ≤ 200（MKT-D-12）；varchar(200) 在 PG 里本就按字符计。
-- 现有技能取默认值（通用场景、无槽位、无预填），行为不变。
alter table agent_skill add column scenes jsonb not null default '[]';
alter table agent_skill add column input_slots jsonb not null default '[]';
alter table agent_skill add column prefill_text varchar(200) not null default '';

-- +goose Down
alter table agent_skill drop column if exists prefill_text;
alter table agent_skill drop column if exists input_slots;
alter table agent_skill drop column if exists scenes;
