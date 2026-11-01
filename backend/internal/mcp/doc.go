// Package mcp 是对外 MCP 服务（Model Context Protocol）的域根。
//
// 只做协议适配与编排（开发 01 MT-1）：工具通过端口调用各域 app 服务，自己不读写其他域的表。
// 子包：domain（纯计算）· app（注册表、主体、端口）· tools（业务工具装配）·
// repo（MCP 自己的表）· transport（HTTP 与 go-sdk 适配）。
// 依赖方向 transport → app → domain、tools → app 端口；任何子包都不 import 其他域的 repo。
package mcp
