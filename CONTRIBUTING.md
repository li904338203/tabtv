# 参与开发

感谢参与 tabtv。交流、注释与文档默认用中文，代码标识符用英文。

## 开工前

1. 读 [`docs/01-需求规格说明书.md`](docs/01-需求规格说明书.md) 确认改动在范围内（§3.10 是明确不做的清单：账号、计价、管理端、MCP、遥测等不接受）。
2. 读对应的设计文档 `docs/dev/0x`。**文末的修订块优先于正文**，两者冲突以修订块为准。
3. 任务卡在 [`docs/tasks/`](docs/tasks/)，看板见 `docs/tasks/README.md`。

## 提交规范

- 分支：`feature/<模块>-<简述>`，不要直推 `main`。
- 提交信息：`type(scope): 中文简述`，type 取 `feat` / `fix` / `refactor` / `perf` / `docs` / `test` / `chore`；有对应任务卡时把卡号写进提交信息（如 `TT-T-32`）。
- 一张卡至少一个提交。实施与设计不一致时，在对应 dev 文档末尾加修订块，不改正文。

## 门禁（每个提交前必须绿，与 `docs/tasks/README.md` §6 一致）

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
