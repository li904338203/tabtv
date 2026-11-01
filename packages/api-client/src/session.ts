/**
 * 会话续期的协调 —— 用户端与管理端**共用这一份**。
 *
 * ⚠️ 后端的 refresh 会**轮换**：每续一次，旧 refresh 立刻作废、发一张新的。由此有三件必须做对的事，
 *    做错任何一件的表现都一样 —— 用户被莫名其妙踢回登录页：
 *
 *  1. **续期后两张都要存**。只存新 access 的话，下一次续期拿着已作废的旧 refresh 必然 40103 ——
 *     用户端移植的是老契约（老后端不轮换），于是登录约 30 分钟就被踢。
 *  2. **同一时刻只续一次**（single-flight）。页面一次发出几个请求、同时撞上 401，各续各的话
 *     只有第一个成功，其余拿着刚被它作废的旧 refresh 失败 —— 15 分钟就被踢。
 *  3. **别的标签页可能刚续过**（cookie / localStorage 是共享的）。本标签页手里的 refresh 已被那边作废时，
 *     先看一眼存储里最新的那一对；是**同一个账号**的就直接采用，而不是续期失败、清会话 ——
 *     清会话会把那个标签页刚写好的新 token 一起删掉，两个标签页一起掉线。
 *     ⚠️ 只采用同一个账号的：另一个标签页可能登录了别的账号，悄悄换成别人的身份更糟。
 */
export interface TokenPairLike {
  accessToken: string
  refreshToken: string
}

export interface StoredTokens {
  accessToken: string | null
  refreshToken: string | null
}

export interface RefresherDeps {
  /** 本标签页内存里的这一对。 */
  read(): StoredTokens
  /** 持久化存储里最新的这一对（cookie / localStorage）—— 别的标签页可能刚写过。 */
  readStored(): StoredTokens
  /** 写入新的一对（内存 + 持久化）。 */
  write(pair: TokenPairLike): void
  /** 调续期接口。⚠️ 不要走带「401 就清会话」的通用请求封装 —— 那会在这里判断之前就把会话清掉。 */
  call(refreshToken: string): Promise<TokenPairLike>
}

/** 从各种请求库的错误里取 HTTP 状态码（ofetch 的 status/statusCode/response.status、自定义错误的 status）；取不到给 0。 */
function statusOf(e: unknown): number {
  if (typeof e !== 'object' || e === null) return 0
  const o = e as { status?: unknown; statusCode?: unknown; response?: { status?: unknown } }
  for (const v of [o.status, o.statusCode, o.response?.status]) {
    if (typeof v === 'number' && v > 0) return v
  }
  return 0
}

/**
 * 续期失败是不是**定论**：续期接口明确回了 4xx（凭证无效 / 已撤销 / 过期 / 类型不对 / 账号被封）。
 *
 * ⚠️ 断网、超时、5xx **不是定论**：refresh 很可能还有效 —— 甚至服务端已经轮换、只是响应没回来，
 *    宽限期内再续一次就能接上。这时清会话等于把一次网络抖动变成「被登出」。
 */
export function isDefinitiveRefreshFailure(e: unknown): boolean {
  const status = statusOf(e)
  return status >= 400 && status < 500
}

/** 取 JWT 的主体（类型 + 用户 ID），只用来判断「是不是同一个账号」，**不验签**（验签是服务端的事）。 */
export function tokenSubject(jwt: string | null | undefined): string {
  if (!jwt) return ''
  const part = jwt.split('.')[1]
  if (!part) return ''
  try {
    const json = atob(part.replace(/-/g, '+').replace(/_/g, '/').padEnd(Math.ceil(part.length / 4) * 4, '='))
    const claims = JSON.parse(json) as { sub?: unknown; knd?: unknown }
    return typeof claims.sub === 'string' ? `${String(claims.knd ?? '')}:${claims.sub}` : ''
  } catch {
    return ''
  }
}

/**
 * 建一个续期函数。返回的 `refresh(stale)`：
 * - `stale` 是失败请求当时带的 access：本标签页的 access 已经不是它了（别的请求刚续过），直接用当前的；
 * - 存储里有同一个账号的、更新的一对（别的标签页刚续过），直接采用；
 * - 否则续期，同一时刻只发一次，其余调用等同一个结果。
 */
export function createTokenRefresher(deps: RefresherDeps): (stale?: string | null) => Promise<string> {
  let inflight: Promise<string> | null = null

  /** 存储里那一对是同一个账号的、且与 `except` 不同（是别人刚续出来的）时采用它。 */
  function adoptStored(except: { access?: string | null; refresh?: string | null }, subject: string): string | null {
    const stored = deps.readStored()
    if (!stored.accessToken || !stored.refreshToken) return null
    if (!subject || tokenSubject(stored.accessToken) !== subject) return null
    if (stored.accessToken === except.access || stored.refreshToken === except.refresh) return null
    deps.write({ accessToken: stored.accessToken, refreshToken: stored.refreshToken })
    return stored.accessToken
  }

  return function refresh(stale?: string | null): Promise<string> {
    const cur = deps.read()
    if (stale && cur.accessToken && cur.accessToken !== stale) return Promise.resolve(cur.accessToken)
    const subject = tokenSubject(stale ?? cur.accessToken)
    const adopted = adoptStored({ access: stale ?? cur.accessToken, refresh: cur.refreshToken }, subject)
    if (adopted) return Promise.resolve(adopted)
    if (inflight) return inflight

    inflight = (async () => {
      const rt = deps.read().refreshToken
      // 没有 refresh 就续不了：这是定论（按 401 记），调用方清会话
      if (!rt) throw Object.assign(new Error('没有 refresh token，需要重新登录'), { status: 401 })
      try {
        const pair = await deps.call(rt)
        // 回了 200 却缺 token：服务端多半已经轮换，宽限期内再续能接上 —— 不是定论（按 502 记）
        if (!pair?.accessToken || !pair?.refreshToken) throw Object.assign(new Error('续期响应缺少 token'), { status: 502 })
        deps.write(pair)
        return pair.accessToken
      } catch (e) {
        // 续期失败之前再看一眼：别的标签页刚好轮换过的话（我们拿的是被它作废的旧 refresh），用它的
        const late = adoptStored({ refresh: rt }, subject || tokenSubject(deps.read().accessToken))
        if (late) return late
        throw e
      }
    })().finally(() => { inflight = null })
    return inflight
  }
}

/**
 * 用 `token` 发一次请求；撞上 401 就续期、用新的 access 重发**一次**。
 *
 * - 续期时带上「发出时用的是哪张」：几个请求同时 401 时只有第一个真的去续，其余直接用续好的那张；
 * - 续期**被明确拒绝**时原样交回那次 401，要不要清会话由调用方决定（用户端取不到一张图不该把人踢出去，
 *   管理端则直接回登录页）；
 * - 续期**没有定论**（断网 / 5xx）时把那个错误抛出去：调用方按网络问题处理、**不清会话**，下一次请求再续
 *   （见 isDefinitiveRefreshFailure）；
 * - 只重发一次：重发仍是 401（比如凭证类型不对）就交回它，不会无限续期。
 */
export async function sendWithRefresh<R extends { status: number }>(
  token: string | null,
  send: (token: string | null) => Promise<R>,
  refresh: (stale: string | null) => Promise<string>,
): Promise<R> {
  const first = await send(token)
  if (first.status !== 401) return first
  let fresh: string
  try {
    fresh = await refresh(token)
  } catch (e) {
    if (isDefinitiveRefreshFailure(e)) return first
    throw e
  }
  return await send(fresh)
}
