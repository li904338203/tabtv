-- +goose Up
-- 起因：创造页的「一键优化提示词」（AGT-43）是一次付费的同步推理调用，但它发生在**还没有会话**的时候
-- （用户还在输入框里改词），挂不到 agent_turn 上（session_id not null，且扫描循环会按会话去推进它）。
--
-- 每次点击记一行，理由与 script_llm_call 相同：
--   - 账本的 refId 用这一行的 id（refType 沿用 agent_turn，与对话费同一种计费口径）：
--     用别的东西做引用的话，第二次优化就撞账本 (ref_id, type) 唯一约束；
--   - **失败也留着**：「这 36 积分花在哪了 / 退了没有」只能从这里回答；
--   - 幂等键作用域「工作区 + 发起人」：同一次点击的网络重试拿回同一个结果，不再扣一次；
--   - 进程在模型返回前崩掉时，行停在 running —— API 的恢复扫描按 created_at 判死并退款
--     （所以 running 有一个部分索引）。
create table agent_prompt_optimize (
  id               bigint primary key,
  workspace_id     bigint not null,
  payer_account_id bigint not null,
  user_id          bigint not null,
  model_code       varchar(64) not null,
  idempotency_key  varchar(128) not null,
  status           varchar(16) not null default 'running',
  credits_cost     integer not null default 0,
  input_text       text not null,
  output_text      text,
  error_code       varchar(64),
  error_message    varchar(1024),
  created_at       timestamptz not null default now(),
  finished_at      timestamptz,
  constraint ck_agent_prompt_optimize_status check (status in ('running','success','failed'))
);
create unique index uk_agent_prompt_optimize_idem
  on agent_prompt_optimize (workspace_id, user_id, idempotency_key);
create index idx_agent_prompt_optimize_running
  on agent_prompt_optimize (created_at) where status = 'running';

-- +goose Down
drop table if exists agent_prompt_optimize;
