-- +goose Up
-- 起因（MCPT-05，对外 MCP 的报价与花钱确认，docs/mcp MCP-F-29/42、MCP-D-05）：外部 AI 客户端
-- 要花用户的积分，必须先让用户看到「这件事要花多少」并确认 —— 而「确认的是哪一次报价」要在
-- 服务端有据可查：只把报价塞进工具结果的话，模型可以改一个参数（多要几张图、换个贵的模型）
-- 再拿着旧的金额来确认，服务端无从分辨。
--
--   mcp_quote —— 每次签发的报价凭证一行。绑定**授权（grant）+ 参数指纹 + 付费主体 + 金额**，
--                10 分钟过期（MCP-PARAM-02，设计建议值，SEEAITV_MCP_QUOTE_TTL）。
--                ⚠️ 落库而不只放 Redis：要可审计（「这次扣费用户确认的是哪张报价」）、
--                可被页面审批（/mcp/approve/{quoteId}，MCPT-13）引用；Redis 挂一次就全丢。
--                ⚠️ fingerprint 是规范化参数的 sha256（mcp/domain.Fingerprint，与调用审计同一个实现），
--                **不存参数原文**：原文里有提示词，审计表不该变成新的泄漏面（detail 里只记文本长度）。
--                ⚠️ used_at 由 MCPT-10 的确认服务用 CAS（where used_at is null）写入：
--                并发重放同一张报价只有一个成功；M1（本迁移）只签发不消费。
--                有任一项不可用时**不签发**（不能让人确认一个跑不了的计划），所以这里不会有「不可用」的行。
--
-- 索引跟着查询走：按授权列最近的报价（审计 / 连接管理页）走 idx_mcp_quote_grant；按 id 取走主键。
-- 过期的行暂不清理：量级是「每次花钱前一行」，远小于调用审计；清理随 MCPT-10 定使用语义后再接。
create table mcp_quote (
  id               bigint primary key,
  grant_id         bigint not null,
  tool             varchar(48) not null,                -- generate（单项）/ plan（计划项）
  fingerprint      bytea not null,                      -- 规范化参数的 sha256（32 字节）
  credits          integer not null,
  payer_account_id bigint not null,
  detail           jsonb not null,                      -- 逐项报价，供确认文案与审计
  expires_at       timestamptz not null,
  approved_at      timestamptz,                         -- 页面审批（url 模式，MCPT-13）
  used_at          timestamptz,                         -- CAS 写入，防重放（MCPT-10）
  created_at       timestamptz not null default now(),
  constraint ck_mcp_quote_credits check (credits >= 0),
  constraint ck_mcp_quote_fingerprint check (octet_length(fingerprint) = 32)
);
create index idx_mcp_quote_grant on mcp_quote (grant_id, created_at desc);

-- +goose Down
drop table mcp_quote;
