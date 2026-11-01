-- 起因：整图复用判定（NEW-05）需要"这次产出对应什么设置"的指纹，
-- 而**从快照反推是行不通的**。
--
-- 最初的设计是：计划器按画布算"现在的设置"的指纹，生成域按
-- `params_snapshot` + `inputs_snapshot` 反算"上次产出用的"指纹，两者比对。
-- 理由是那两列本来就记着这次任务用了什么，不必再加列。
--
-- 联调把这个推理证伪了：两侧的**表示形态根本不同** ——
--   · 计划器手上是画布节点的 params 与"上游节点 ID"；
--   · 生成域手上是受理后**解析过**的 params（可能追加了素材词）
--     与解析后的输入（产出 ID、媒体 ID，而不是节点 ID）。
-- 于是同一次生成在两边算出两个指纹，复用**永远不成立**：每次运行都把
-- 所有节点重跑一遍，用户每跑一次图就多花一次钱，而系统没有任何报错。
--
-- 这正是"同一个语义有两份实现"的典型：不是实现写错了，是它们根本
-- 看不到同一份输入。改成**算一次、存下来**：受理时把计划器算好的值写进来，
-- 读的时候直接比。
--
-- ⚠️ 为空表示"不知道这次产出对应什么设置"，一律**不复用**（宁可多跑一次，
-- 也不能把一张对不上的图当成本次结果）。手工单点运行的任务目前就是空的，
-- 它们的复用随 P08-03 一起接。

-- +goose Up
alter table generation add column input_fingerprint varchar(16);

comment on column generation.input_fingerprint is
    '产出对应的输入指纹（workflow/domain.Fingerprint 算出）；为空表示不可复用';

-- 计划器按 project + node 查最近一次成功产出的指纹。
create index idx_generation_node_fp on generation (project_id, node_id)
    where status = 'success' and node_id is not null;

-- +goose Down
drop index if exists idx_generation_node_fp;
alter table generation drop column if exists input_fingerprint;
