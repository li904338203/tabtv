// @tabtv/api-client —— 由 api/ 契约生成的类型与客户端。
//
// ⚠️ P01-04 接入 `make gen` 后，本目录的 generated/ 内容**禁止手改**；
// 契约漂移由 CI 的 `make gen-check` 拦截。
//
// ⚠️ 生成物（`generated/api.ts`）**就是新契约的唯一类型来源**。
// 用户端目前还留着一份 `frontend/types/legacy`（老 Java 契约的抄本，移植期的脚手架）——
// 两份类型并存时，"读对了新字段反而编译不过"是常态（本次画布参数分区
// `scope` → `group` 就撞在这上面）。新写的代码一律从这里取类型，
// legacy 那份只出不进、逐步清零。
export type { components, paths, operations } from './generated/api'
/**
 * 业务错误码（由 api/errors.yaml 生成）。按码分流文案时从这里取，不在各处写死数字 ——
 * 改了码表而某处还写着旧数字时，那一处的文案会静默退回兜底（3D 导演台 DRT-20 起用）。
 */
export { ErrorCode } from './generated/errors'

/** 工作区作用域请求头。服务端两条都校验：调用者是成员 且 资源归属一致。 */
export const HEADER_WORKSPACE_ID = 'X-Workspace-Id'
/** 写钱、建任务、建订单必须携带。 */
export const HEADER_IDEMPOTENCY_KEY = 'Idempotency-Key'
/** 全链路追踪 ID，成功响应也带（信封已取消，见 CD-02）。 */
export const HEADER_TRACE_ID = 'X-Trace-Id'

/** 会话续期的协调（单飞 · 存双 token · 跨标签页）：用户端与管理端共用，见 session.ts。 */
export { createTokenRefresher, isDefinitiveRefreshFailure, sendWithRefresh, tokenSubject } from './session'
export type { RefresherDeps, StoredTokens, TokenPairLike } from './session'
