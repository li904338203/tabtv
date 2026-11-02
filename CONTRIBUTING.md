# 参与开发

感谢参与 tabtv。交流、注释与文档默认用中文，代码标识符用英文。

## 范围

tabtv 是单机桌面应用：账号、计价、管理端、MCP、遥测等不在范围内，相关改动不接受。

## 提交规范

- 分支：`feature/<模块>-<简述>`，不要直推 `main`。
- 提交信息：`type(scope): 中文简述`，type 取 `feat` / `fix` / `refactor` / `perf` / `docs` / `test` / `chore`。

## 门禁（每个提交前必须绿）

| 项 | 要求 |
| --- | --- |
| 门禁 | `make check`（内部信息扫描 + fmt + lint + vet + typecheck + test + build + gen-check） |
| 集成测试 | 改到 repo 或迁移时跑 `make test-it`（内嵌 PostgreSQL，不需要 docker） |
| 迁移 | 新迁移从 `00082` 起顺序编号，不预留号；表数绊线随迁移同步改；不改历史迁移 |
| 契约 | 改接口先改 `api/openapi.yaml` 再 `make gen`，禁止手写 DTO |
| 前端 | `pnpm --filter @tabtv/web test` 与 typecheck 绿；保留页面不改样式 |
| 三平台 | 涉及 Wails 绑定、文件路径、子进程、打包的改动，CI 三平台都要绿 |
| 假上游 | 链路类验证一律用 `scripts/fake-upstream`，不依赖真实 Key |
| 许可 | 新增依赖后跑 `bash build/gen-licenses.sh`，出现 GPL / AGPL 即不可合入 |

## 不要提交的东西

- 任何 API Key、密码、私有服务地址。`make check` 里的 `scripts/scan-internal.sh` 会扫一遍，但它只是兜底。
- 下载产物：`build/third_party/`、`backend/assets/fonts/` 下的字体、`build/bin/`（已在 `.gitignore`）。

## 许可

提交即表示你同意以 [Apache License 2.0](LICENSE) 授权你的贡献。
