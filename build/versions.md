# 版本基线（TT-T-01）

| 工具 | 版本 | 依据 |
| --- | --- | --- |
| Go | `go 1.26.0`（go.mod；本机 1.25.4 由工具链自动下载 1.26.0） | 来源 backend `go.mod` |
| Node | 20.20.0（`.nvmrc`、`package.json` engines） | 来源基线 |
| pnpm | 10.32.1（`packageManager`） | 来源基线 |
| Wails | v2（CLI 版本在 TT-T-03 安装时锁定） | dev/01 TTD-1 |
| PostgreSQL | 17（内嵌，TT-T-04） | 来源测试 `postgres:17-alpine` |
| ffmpeg | 8.1.3。macOS 从源码静态编（`build/ffmpeg/build-macos.sh`，LGPL-2.1）；Windows / Linux 重打包 BtbN `lgpl-shared`（`build/ffmpeg/repack-btbn.sh`，LGPL-3.0）；镜像到本仓库 Release `third-party-ffmpeg-8.1.3`，锁在 `build/third_party.lock` | dev/04 TTD-52、R-6 |
