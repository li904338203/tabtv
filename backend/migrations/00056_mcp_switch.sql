-- +goose Up
-- 起因（MCPT-14，对外 MCP 的两级开关，docs/mcp dev/01 §12 R7、dev/02 §5）：
--
--   workspace.mcp_disabled —— 工作区级开关：工作区 owner / admin 可以关掉本工作区的 MCP。
--                             关闭**不撤销** grant（重新打开即恢复）；关着时外部客户端的调用回
--                             403 MCP_DISABLED，**站内功能完全不受影响**。默认 false（开）：
--                             存量工作区不能因为这次迁移就一起断掉。
--   mcp_setting            —— 平台级运行时开关（单行表）。SEEAITV_MCP_ENABLED 是启动时环境变量，
--                             出事时改不了（要发版 / 重启）；总开关的用途恰恰是**止损**，所以要能在
--                             管理端即时关掉。关着时 /mcp* 与 OAuth 元数据 / 授权端点一律 404
--                             （关掉就是不存在），同样不撤销任何 grant。
--
-- ⚠️ mcp_setting 的主键**写死 id=1**（ck_mcp_setting_single）：每次现取雪花 ID 的话
-- `on conflict (id)` 永远不命中、保存一次就多一行，而读取拿到的是最早那行 ——
-- 症状是「后台关了 MCP、保存成功、而外部调用照样通」（CLAUDE.md §5.10 render_setting 踩过）。
-- 不预插行：没有这一行 = 从没改过 = 开启（与读不到时的取向一致，见 mcp/app.Switch）。
alter table workspace add column mcp_disabled boolean not null default false;

create table mcp_setting (
  id                  smallint primary key,
  enabled             boolean not null default true,
  version             integer not null default 1,
  updated_by_admin_id bigint,
  updated_at          timestamptz not null default now(),
  constraint ck_mcp_setting_single check (id = 1)
);

-- +goose Down
drop table mcp_setting;
alter table workspace drop column mcp_disabled;
