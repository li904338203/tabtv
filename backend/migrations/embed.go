// Package migrations 以 embed.FS 承载全部 SQL 迁移。
//
// 新库从 00001 直接建最终结构，**不重放旧 Flyway 的 V1~V60**（03 §7）。
// 文件按数据模型 §16 的初始化依赖分层，每层可独立审阅。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

// Dir 是 embed.FS 内的相对目录。
const Dir = "."
