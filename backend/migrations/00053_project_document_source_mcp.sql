-- 项目文档的写入来源加 mcp（MCPT-11；MCP-F-26、开发 01 §12 R17）
--
-- 起因：外部 AI 客户端经 MCP 的 documents.put 写项目文档（与站内 Agent 写 /plan.md 同一张表、
-- 同一个乐观锁）。但 project_document.source 的 CHECK 只允许 agent / user（00037）：
--
--   * 记成 agent 是错的 —— 没有 agent 会话，前端会显示「Agent 写入」；
--   * 记成 user 的话事后分不清是用户本人改的还是他授权的客户端改的，冲突提示也会把
--     MCP 的写入说成「你在另一个窗口修改过」（bySelf=true），用户点「以我为准」就覆盖掉
--     客户端刚写的内容 —— 与画布那边 MCPT-09 修掉的是同一个缺陷。
--
-- 只放宽约束，不加表、不加列（表数不变）。MCP 写入时 last_editor_user_id 仍是授权所属的用户本人
-- （「代谁写的」），哪个授权写的不落在这张表上：canvas 域不读 mcp_* 表，排障走 mcp_call_log。
-- +goose Up
alter table project_document drop constraint ck_project_document_source;
alter table project_document add constraint ck_project_document_source
  check (source in ('agent', 'user', 'mcp'));

-- +goose Down
-- ⚠️ 先把已有的 mcp 行改成 user 再收紧约束：留着的话加回约束直接失败，回退做不下去。
-- 不删行：文档是用户的内容（删掉不可逆）；MCP 本来就是代 last_editor_user_id 那个人写的，
-- 记成 user 只丢了「经哪个客户端」这一层信息。
update project_document set source = 'user' where source = 'mcp';
alter table project_document drop constraint ck_project_document_source;
alter table project_document add constraint ck_project_document_source
  check (source in ('agent', 'user'));
