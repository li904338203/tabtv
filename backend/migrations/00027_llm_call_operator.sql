-- 同步 LLM 调用要记得住是谁发起的（P12-03；NEW-20、FULL-06）
--
-- 起因：NEW-20 要求「生成/编辑/导出记录工作区、操作者和费用承担账户」。
-- 逐表核对下来只有 `script_llm_call` 缺操作者：
--   generation / render_job / workflow_run 都有 created_by_user_id，
--   export_task 有 user_id，
--   而分镜与提示词合成走的是**同步**文本调用、费用记在这张表上 ——
--   它是唯一一条「花了钱却说不出是谁花的」的链。
--
-- 账本那侧其实记得住（`credit_transaction.operator_user_id`），但要从
-- 「这次分镜是谁跑的」问到答案得先拿 callID 去流水里反查一次。
-- 团队工作区里排查「这 60 积分谁花的」是个常见问题，多一次反查就多一次出错机会。
--
-- ⚠️ 允许为空：P12-03 之前的历史行没有这个信息，**不编造**一个。
-- 查询侧按「未知」显示，而不是默认成工作区所有者 —— 后者会让审计指向一个
-- 完全无辜的人，而且看不出那是猜的。
-- +goose Up
alter table script_llm_call add column created_by_user_id bigint;

-- +goose Down
alter table script_llm_call drop column created_by_user_id;
