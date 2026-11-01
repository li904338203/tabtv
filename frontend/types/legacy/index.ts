/**
 * ⚠️ **老契约的类型，临时 vendored（P15-01 前端移植）**。
 *
 * 这份 `api.ts` 是从**老 Java 后端**的 OpenAPI 生成的，描述的是老契约 ——
 * 与新 Go 后端**不一致**（信封已取消、路径与字段名都有出入，见 CD-02）。
 *
 * 它存在的唯一理由是：移植过来的 41 处 `import type { components }` 要先
 * 编译得过，否则"让前端先跑起来"会变成一场类型大战。
 *
 * ⚠️ **这不是对老仓库的依赖** —— 是一份拷贝进来的 `.ts` 文件，
 * 新项目的构建不碰上级目录（CLAUDE.md 的铁律）。
 *
 * ⚠️ **要逐步换成 `@tabtv/api-client`**：每重接一个 API，就把那处的
 * 类型从这里迁走。迁完这个目录要删掉 —— 留着它等于让前端照着一份
 * **已经不成立的契约**做类型检查，而类型检查通过会让人以为对齐了。
 * 进度记在 docs/00 的 P15-01 挂账里。
 */
export type ApiResult<T> = {
  code: number
  message: string
  data: T
  traceId: string
}

export * from './api'
