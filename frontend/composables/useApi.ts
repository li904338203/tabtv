/**
 * API 层 —— **对接新 Go 后端**（P15-01 前端移植）。
 *
 * 老前端是对着老 Java 后端写的，两处根本差异都收敛在这一个文件里，
 * 所以 114 个调用点基本不用动：
 *
 *  1. **信封没有了**（CD-02）。老后端一律 HTTP 200，成败看 body 里的
 *     `Result<T>{code,message,data,traceId}`，成功要 `.data` 解一层。
 *     新后端**直接返回业务 JSON**，失败靠 **HTTP 状态码**，
 *     body 是 `{code,name,message,traceId}`。
 *     ⚠️ 所以这里**不能再解 `.data`** —— 解了的话每个接口都会拿到 undefined，
 *     而那表现为"页面全是空的"，不是报错。
 *
 *  2. **多了工作区**。新后端按 `X-Workspace-Id` 分租户，而老系统没有这个概念
 *     （它按用户分）。⚠️ 漏发这个头的表现是 **400「缺少 X-Workspace-Id 请求头」**，
 *     而不是 401 —— 看起来像参数写错了。
 *
 * 401 的处置沿用老系统（refresh 一次 → 不行就清会话回登录页），
 * 那套是对的：⚠️ **只有 401 才踢**，断网/403/5xx 都不踢
 * （断网时踢过去，用户会以为被登出然后重新输一次密码，而那次也会失败）。
 */

import { isDefinitiveRefreshFailure } from '@tabtv/api-client'

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
  const auth = useAuthStore()
  const workspace = useWorkspaceStore()
  const lastTraceId = useState<string | null>('api-trace-id', () => null)

  async function request<T>(
    path: string,
    options: FetchOptions,
    allowRefresh: boolean,
  ): Promise<T> {
    // ⚠️ 工作区还没取到就先取（store 里单飞）：页面加载时那一次若因网络问题失败，之后每个请求都缺工作区头（400），
    //    要等下一次换页才会重取 —— 用户只看到一串「缺少 X-Workspace-Id」。
    if (import.meta.client && !workspace.loaded && auth.accessToken) await workspace.load()
    const headers = new Headers(options.headers as HeadersInit | undefined)
    // 记下这次带的是哪张 access：撞上 401 时，若它已经被别的请求续掉了，直接用新的重试，不再续一次。
    const sentToken = auth.accessToken
    if (sentToken) headers.set('Authorization', `Bearer ${sentToken}`)
    // ⚠️ 工作区头：新后端几乎每个业务端点都要。
    // 认证类端点（/auth/*）不需要，多发一个也无害，所以不做例外判断 ——
    // 做例外判断就等于维护第二份"哪些端点要工作区"的清单，而那份清单会过时。
    if (workspace.currentId) headers.set('X-Workspace-Id', workspace.currentId)

    try {
      // ⚠️ 返回的就是业务对象本身，**不解 `.data`**。
      const data = await $fetch<T>(`${config.public.apiBase}${path}`, {
        ...options,
        headers,
      })
      return data
    } catch (cause) {
      const failure = failureResponse(cause)
      const body = failure._data
      if (body?.traceId) lastTraceId.value = body.traceId

      if (
        failure.status === 401 &&
        allowRefresh &&
        path !== '/auth/refresh' &&
        auth.refreshToken
      ) {
        let refreshed = false
        try {
          await auth.refreshAccessToken(sentToken)
          refreshed = true
        } catch (e) {
          // ⚠️ 断网 / 5xx 续不上**不是定论**：不清会话，按网络问题报给页面；下一次请求会再续
          //    （服务端已经轮换、只是响应没回来的话，宽限期内拿旧 refresh 也接得上）。
          //    只有续期接口明确拒绝（refresh 被撤销 / 过期 / 账号被封）才落到下面的 401 处置。
          if (!isDefinitiveRefreshFailure(e)) {
            throw createError({ statusCode: 503, message: '网络不稳定，登录状态暂时无法续期，请稍后重试' })
          }
        }
        // ⚠️ 重试放在 try 外：重试本身失败（比如 403）不能被当成「续期失败」—— 那会把一个没权限的请求
        //    变成「清会话、回登录页」。
        if (refreshed) return request<T>(path, options, false)
      }

      if (failure.status === 401) {
        auth.clearSession()
        if (import.meta.client) await navigateTo('/login')
      }

      // ⚠️ **40302 = 工作区 ID 陈旧**（不是真的没权限）：最常见的成因是
      //    同一个标签页换过账号，而 localStorage 里还存着上一个账号的工作区。
      //    这里自愈一次：清掉重新拉，再重试同一个请求。
      //
      //    ⚠️ 只重试**一次**（allowRefresh 复用同一个开关）：真的不是成员时
      //    重拉之后还会失败，无限重试会把一次越权变成一场请求风暴。
      if (body?.code === 40302 && allowRefresh && import.meta.client) {
        workspace.clear()
        await workspace.load()
        if (workspace.currentId) return request<T>(path, options, false)
      }

      throw createError({
        statusCode: failure.status ?? 500,
        message: body?.message ?? '请求失败',
        // ⚠️ details 要带出去：冲突条靠它说出「是谁改的」（CD-58），丢了就只能退回中性说法
        data: { code: body?.code, name: body?.name, traceId: body?.traceId, details: body?.details },
      })
    }
  }

  return function api<T>(path: string, options: FetchOptions = {}) {
    return request<T>(path, options, true)
  }
}
