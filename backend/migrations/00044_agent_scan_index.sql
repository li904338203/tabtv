-- 画布 Agent 扫描循环的两条部分索引（AGT-25；AG-04/08，docs/agent dev/01 §4.3）
--
-- 起因：扫描循环每 2 秒跑一次，00040 只为「运行态会话 / running 轮 / pending 卡片」建了索引，
-- 另外两类候选没有：
--   1. pending 的轮：运行中发的消息排成下一轮，那条队列消息若在 Worker 持有租约时到达会被
--      当作 Busy 丢弃（AGT-22 已知缺口），之后没有任何人再投它 —— 扫描要按 pending 轮把它捞回来。
--      pending 轮在正常情况下几秒内就变成 running，集合很小，部分索引几乎不占空间。
--   2. charged 的额度条目：生成失败、生成域退款之后要把条目标 refunded（额度归还）。
--      查询带 7 天窗口（同 generation 的补退款扫描），索引按 updated_at 才能只扫窗口内的行；
--      不建的话每 2 秒全表扫一遍会随 Agent 的付费调用次数线性变慢。
-- 只加索引，不改表结构。
-- +goose Up
create index idx_agent_turn_pending on agent_turn (created_at) where status = 'pending';
create index idx_agent_budget_charged on agent_budget_entry (updated_at) where status = 'charged';

-- +goose Down
drop index if exists idx_agent_budget_charged;
drop index if exists idx_agent_turn_pending;
