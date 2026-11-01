-- +goose Up
-- 起因（AGT-50 多段工具）：额度条目原先只有 generation_id —— 第一期唯一的付费工具 generate
-- 一次只受理一个生成任务。多段工具挂的对象不只是生成任务：
--   - 分镜 / 提示词合成是同步文本调用，费用记在 script_llm_call（一次合成可能是几十次调用）；
--   - 资产批量出图、组执行是一次工具调用受理**一批**生成任务；
--   - 整图运行的子任务要到运行跑起来才陆续出现；渲染任务的流水 ref_type 是 render。
-- 扫描循环据 generation_id 回写 refunded 时这些一个都找不到，额度永远停在「已占用」。
--
-- 更要紧的是**组可能部分成功**：10 个子项失败 3 个，生成域按原批次退回那 3 个的钱，
-- 而条目只有 charged / refunded 两种整体状态 —— 标 charged 的话 Agent 的「还剩多少授权」
-- 比账本少了 3 份，标 refunded 又把成功的 7 份当没花。所以退回要按金额记（refunded 列）。
--
--   ref_type / ref_id —— 条目关联的业务对象（generation / script_llm_call / script_compose /
--                        asset_batch / script_group / workflow_run / render_job）；
--   ref_ids           —— 一次调用受理的那一批对象（生成任务 ID、同步调用 ID）。结算时据它逐笔
--                        对账本（只认 credit_transaction，不认业务表上的 credits_cost 报价）；
--   refunded          —— 已退回的积分（部分退回累加；占用口径 = quoted − refunded）；
--   settled_at        —— 已按账本结算（扫描循环不再处理它）。部分退回的条目仍是 charged，
--                        没有这一列就分不出「结算过、退了一部分」与「还没结算」。
--
-- generation_id 保留：第一期 generate 的条目、扫描 E 的候选查询与取消路径都按它找任务。
alter table agent_budget_entry add column ref_type varchar(32);
alter table agent_budget_entry add column ref_id bigint;
alter table agent_budget_entry add column ref_ids bigint[];
alter table agent_budget_entry add column refunded integer not null default 0;
alter table agent_budget_entry add column settled_at timestamptz;
alter table agent_budget_entry add constraint ck_agent_budget_refunded
  check (refunded >= 0 and refunded <= quoted);
alter table agent_budget_entry add constraint ck_agent_budget_ref_type
  check (ref_type is null or ref_type in ('generation','script_llm_call','script_compose',
    'asset_batch','script_group','workflow_run','render_job'));

-- 已有条目都是 generate 的：补上引用，之后两条路径按同一套列读。
update agent_budget_entry set ref_type = 'generation', ref_id = generation_id
  where generation_id is not null and ref_type is null;

-- 扫描循环的「待结算」候选：charged 且关联了非生成对象、还没结算。集合很小（正常几秒到
-- 几分钟就结算掉），部分索引只装这几行；不建的话每 2 秒按 status 扫全部 charged 条目。
create index idx_agent_budget_unsettled on agent_budget_entry (updated_at)
  where status = 'charged' and settled_at is null and ref_type is not null and ref_type <> 'generation';

-- +goose Down
drop index if exists idx_agent_budget_unsettled;
alter table agent_budget_entry drop constraint if exists ck_agent_budget_ref_type;
alter table agent_budget_entry drop constraint if exists ck_agent_budget_refunded;
alter table agent_budget_entry drop column if exists settled_at;
alter table agent_budget_entry drop column if exists refunded;
alter table agent_budget_entry drop column if exists ref_ids;
alter table agent_budget_entry drop column if exists ref_id;
alter table agent_budget_entry drop column if exists ref_type;
