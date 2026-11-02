/**
 * API 层（TT-T-15）。
 *
 * 桌面版的后端在同一个进程里，经 Wails 的 AssetServer 同源提供 `/api/v1`（dev/01 TTD-2）：
 *  - 不发 `Authorization` / `X-Workspace-Id`：后端给每个请求自动补上本机身份（desktop/local.go）；
 *  - 没有 401 续期与跳登录、没有 40302 工作区自愈 —— 本机身份不会过期，也只有一个工作区。
 *
 * 保留的两条来源系统约定：
 *  1. **没有信封**：后端直接返回业务 JSON，失败靠 HTTP 状态码，body 是 `{code,name,message,traceId,details}`。
 *     ⚠️ 这里**不能解 `.data`** —— 解了的话每个接口都拿到 undefined，表现为「页面全是空的」而不是报错。
 *  2. 失败时把 `details` 带出去（版本冲突要靠它说出「是谁改的」，CD-58）。
 */

type FetchOptions = NonNullable<Parameters<typeof $fetch>[1]>

/** 新后端的错误体。成功响应**没有**这层包装。 */
type ErrorBody = {
  code?: number
  name?: string
  message?: string
  traceId?: string
  /** 附加信息袋子（如版本冲突的 currentEditorName / bySelf，契约 VersionConflictError） */
  details?: Record<string, unknown>
}

type FailureResponse = {
  status?: number
  _data?: ErrorBody
}

function failureResponse(error: unknown): FailureResponse {
  if (typeof error !== 'object' || error === null || !('response' in error)) return {}
  return (error as { response?: FailureResponse }).response ?? {}
}

export function useApi() {
  const config = useRuntimeConfig()
  const lastTraceId = useState<string | null>('api-trace-id', () => null)

  return async function api<T>(path: string, options: FetchOptions = {}): Promise<T> {
    try {
      // ⚠️ 返回的就是业务对象本身，**不解 `.data`**。
      return await $fetch<T>(`${config.public.apiBase}${path}`, options)
    } catch (cause) {
      const failure = failureResponse(cause)
      const body = failure._data
      if (body?.traceId) lastTraceId.value = body.traceId
      throw createError({
        statusCode: failure.status ?? 500,
        message: body?.message ?? '请求失败',
        data: { code: body?.code, name: body?.name, traceId: body?.traceId, details: body?.details },
      })
    }
  }
}
