// Package repo 是 MCP 域自己的表（mcp_grant、mcp_access_key，以及后续的 mcp_quote、mcp_call_log……）的仓储。
//
// ⚠️ 只放 MCP 自己的表；其他域的数据（成员角色、账号状态、会员等级）一律经 app 端口取（MT-1）。
package repo
