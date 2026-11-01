# 版本基线（TT-T-01）

| 工具 | 版本 | 依据 |
| --- | --- | --- |
| Go | `go 1.26.0`（go.mod；本机 1.25.4 由工具链自动下载 1.26.0） | 来源 backend `go.mod` |
| Node | 20.20.0（`.nvmrc`、`package.json` engines） | 来源基线 |
| pnpm | 10.32.1（`packageManager`） | 来源基线 |
| Wails | v2（CLI 版本在 TT-T-03 安装时锁定） | dev/01 TTD-1 |
| PostgreSQL | 17（内嵌，TT-T-04） | 来源测试 `postgres:17-alpine` |
| ffmpeg | LGPL 构建（TT-T-26 锁定 URL 与 SHA-256） | dev/04 TTD-52 |
