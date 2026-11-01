-- 画布来源泛化：MCP 授权成为画布操作的第三种发起方（MCPT-09；MCP-F-60/61/62、MCP-D-07、MT-6）
--
-- 起因：外部 AI 客户端（Claude Code、Cursor……）经 MCP 往画布上建节点，走的是与站内 Agent
-- 同一条 ApplyOps 路径（服务端在最新版本上应用一批操作 + canvas_op_log + canvas.ops_applied）。
-- 但这条路径的「发起方」此前只有 agent / user 两种：
--
--   * canvas_op_log.actor_kind 的 CHECK 只允许 agent / user —— MCP 批次写不进日志，
--     打开着画布的浏览器也就无从补取重放；
--   * 冲突判定（CanvasState）只看 last_editor_agent_session_id：MCP 写入时它为空、
--     last_editor_user_id 是授权所属的用户本人 → editorKind=user 且 bySelf=true，
--     前端会弹「你在另一个窗口修改过」，用户点「以我为准」就把 MCP 刚建的节点整份覆盖
--     （违反 MCP-F-60）。所以 project 也要记「最后一次写入来自哪个 MCP 授权」。
--
-- 只加列不加表（表数不变）。两列都允许为空：历史行没有这个信息，**不编造**。
-- ⚠️ 真人整份保存（SaveCanvas）要把 last_editor_mcp_grant_id 置空，与 agent 会话同一条规则 ——
-- 不清的话冲突详情会把一次真人保存说成「外部客户端改的」，前端据此自动重存，把同事的内容覆盖掉。
-- mcp_grant_id 是逻辑外键（mcp_grant.id），canvas 域不读 mcp_* 表。
-- +goose Up
alter table canvas_op_log drop constraint ck_canvas_op_actor;
alter table canvas_op_log add constraint ck_canvas_op_actor
  check (actor_kind in ('agent', 'user', 'mcp'));
alter table canvas_op_log add column mcp_grant_id bigint;

alter table project add column last_editor_mcp_grant_id bigint;

-- +goose Down
-- ⚠️ 先删 MCP 批次再收紧约束：留着的话加回约束直接失败，回退做不下去。
-- 操作日志只是重放缓存（保留 30 天），删掉的区间前端补取时得到 needsReload、整页重载即可。
alter table project drop column last_editor_mcp_grant_id;
delete from canvas_op_log where actor_kind = 'mcp';
alter table canvas_op_log drop column mcp_grant_id;
alter table canvas_op_log drop constraint ck_canvas_op_actor;
alter table canvas_op_log add constraint ck_canvas_op_actor
  check (actor_kind in ('agent', 'user'));
