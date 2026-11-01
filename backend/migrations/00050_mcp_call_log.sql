-- +goose Up
-- 起因（MCPT-06，对外 MCP 的调用审计，docs/mcp MCP-F-70/71、MCP-D-06）：外部 AI 客户端拿着用户的
-- 授权在工作区里读写、花钱，「这次调用是谁、用哪个客户端、调了什么、结果如何」必须事后查得到 ——
-- 平台审计（audit_log）只记密钥 / 授权的创建与撤销，记不了每一次工具调用。
--
--   mcp_call_log —— 每次 tools/call 一行，**含被拒（denied）与被限流（rate_limited）的**：
--                   滥用与越权尝试恰恰是最需要留痕的那一类，只记成功的话它们一条都看不到。
--                   ⚠️ 参数**只存指纹与脱敏摘要**（MCP-N-03）：原文里可能有提示词、用户贴进来的密钥、
--                   base64 图片 —— 存原文等于把审计表变成新的泄漏面，也会把表撑爆。
--                   ⚠️ **会无限增长**：Worker 每小时的日志清理循环把它纳入，保留 30 天（M-7 已定），
--                   分批删除（一次删几十万行会长时间持锁，拖慢正在写日志的请求）。
--                   写入是异步的、失败只告警：审计写不进去不能让工具调用失败（MCP-N-06）。
--
-- 索引跟着查询走（CLAUDE.md §5.25）：清理按 created_at 删（idx_mcp_call_log_created）；
-- 按授权查调用记录（MCPT-14 的连接管理页）按 id 倒序游标翻页（idx_mcp_call_log_grant）。
create table mcp_call_log (
  id               bigint primary key,
  grant_id         bigint not null,
  user_id          bigint not null,
  workspace_id     bigint not null,
  client_name      varchar(100) not null,
  tool             varchar(48) not null,
  method           varchar(32),
  args_fingerprint bytea,                                -- sha256(规范化参数)，同参数同指纹
  args_summary     text,                                 -- logredact 脱敏 + 截断
  credits          integer not null default 0,           -- 本次调用实际花费（M1 恒 0）
  result           varchar(16) not null,
  error_code       integer,
  duration_ms      integer not null,
  trace_id         varchar(64),
  created_at       timestamptz not null default now(),
  constraint ck_mcp_call_log_result check (result in ('ok','error','needs_confirm','denied','rate_limited')),
  constraint ck_mcp_call_log_credits check (credits >= 0),
  constraint ck_mcp_call_log_duration check (duration_ms >= 0)
);
create index idx_mcp_call_log_created on mcp_call_log (created_at);
create index idx_mcp_call_log_grant on mcp_call_log (grant_id, id desc);

-- +goose Down
drop table mcp_call_log;
