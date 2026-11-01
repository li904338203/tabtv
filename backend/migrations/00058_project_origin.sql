-- +goose Up
-- 起因（MKT-T-16 / MKD-3 / MK-17）：营销项目要按「从哪个入口建的」判定。这是**创建时的事实**，
-- 事后算不出来（营销项目与普通 Agent 项目一样有 agent_session），所以必须落列。
--
-- ⚠️ 只允许 'marketing' 或空：其余入口（创造页、空白画布、MCP）现在没有人读它们的来源，
--    各写一个值就是「建了没人用」的字段，而漏写时没有任何报错。需要时再扩 CHECK。
-- ⚠️ 历史项目为空 = 非营销，不回填。
-- ⚠️ 建后不可改：没有任何更新路径写它；PATCH /projects/{id} 收到 origin 字段回 400（MKT-AC-21③）。
-- ⚠️ 与 kind=agent（按 agent_session 现算，AGT-45）并存：营销按落列判定，Agent 仍按会话现算。
alter table project add column origin varchar(16);
alter table project add constraint ck_project_origin
  check (origin is null or origin in ('marketing'));
-- 部分索引：绝大多数项目 origin 为空，只为营销筛选那一小部分建索引。
-- 列序与列表的排序键一致（workspace_id 等值 → updated_at 倒序）。
create index idx_project_origin on project (workspace_id, origin, updated_at desc)
  where origin is not null;

-- +goose Down
drop index if exists idx_project_origin;
alter table project drop constraint if exists ck_project_origin;
alter table project drop column if exists origin;
