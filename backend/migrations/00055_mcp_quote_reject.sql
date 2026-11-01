-- +goose Up
-- 起因（MCPT-13，对外 MCP 的页面审批，MCP-F-44、MCP-AC-15）：凭据开了「花钱需在 seeAitv 页面确认」时，
-- 花钱工具签发的报价要由**发起授权的用户本人**在 /mcp/approve/{quoteId} 上批准或拒绝。
-- 契约有 POST /mcp/approvals/{quoteId}/reject，而 00051 只给了 approved_at —— 「拒绝」没有地方落：
--
--   * 不落库的话拒绝等于什么都没发生，客户端带着同一个 quoteId 再来一次，用户刚点过的「拒绝」就被
--     一次自动确认绕过去了（确认服务只认 approved_at，拒绝必须让它**直接无效**）；
--   * 拿 used_at 代替也不行：used_at 的语义是「已受理或已作废」，已用的 quote 再提交走**重放**
--     （MCPT-10，开发 01 §12 R14），拒绝的 quote 被当成已用去重放，同一幂等键下查不到任务，
--     就会被正常受理一次 —— 用户明明拒绝了，钱照样花了。
--
-- 只加一列（表数不变）。两条 CHECK 把三种结局钉成互斥：批准与拒绝不能同时成立；拒绝的不能再被用掉。
-- 写入一律 CAS（mcp/repo：approved_at / rejected_at / used_at 都为空且未过期才写），CHECK 是兜底。
alter table mcp_quote add column rejected_at timestamptz;
alter table mcp_quote add constraint ck_mcp_quote_decided
  check (approved_at is null or rejected_at is null);
alter table mcp_quote add constraint ck_mcp_quote_rejected_unused
  check (rejected_at is null or used_at is null);

-- +goose Down
alter table mcp_quote drop constraint ck_mcp_quote_rejected_unused;
alter table mcp_quote drop constraint ck_mcp_quote_decided;
alter table mcp_quote drop column rejected_at;
