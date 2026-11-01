import { describe, expect, it } from 'vitest'

import { createTokenRefresher, isDefinitiveRefreshFailure, sendWithRefresh, tokenSubject, type StoredTokens, type TokenPairLike } from './session'

/** 造一个只有载荷的假 JWT（不验签，只用来区分账号）。 */
function jwt(sub: string, n: number, knd = 'user'): string {
  const b64 = (o: unknown) => btoa(JSON.stringify(o)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  return `${b64({ alg: 'HS256' })}.${b64({ sub, knd, n })}.sig`
}

/** 一个标签页：内存里的一对 + 与其他标签页共享的存储。 */
function tab(storage: { pair: StoredTokens }, initial: StoredTokens, server: (rt: string) => Promise<TokenPairLike>) {
  let mem = { ...initial }
  const calls: string[] = []
  const refresh = createTokenRefresher({
    read: () => mem,
    readStored: () => storage.pair,
    write: (p) => { mem = { ...p }; storage.pair = { ...p } },
    call: (rt) => { calls.push(rt); return server(rt) },
  })
  return { refresh, calls, mem: () => mem }
}

/** 模拟会轮换的后端：每张 refresh 只能用一次。 */
function rotatingServer(sub: string) {
  let n = 1
  const valid = new Set([`R1-${sub}`])
  return async (rt: string): Promise<TokenPairLike> => {
    await new Promise((r) => setTimeout(r, 5))
    if (!valid.has(rt)) throw Object.assign(new Error('凭证已被撤销'), { status: 401 })
    valid.delete(rt)
    n += 1
    const next = { accessToken: jwt(sub, n), refreshToken: `R${n}-${sub}` }
    valid.add(next.refreshToken)
    return next
  }
}

describe('tokenSubject', () => {
  it('取出类型 + 用户 ID；坏的 token 给空串', () => {
    expect(tokenSubject(jwt('42', 1))).toBe('user:42')
    expect(tokenSubject(jwt('42', 1, 'admin'))).toBe('admin:42')
    expect(tokenSubject('not-a-jwt')).toBe('')
    expect(tokenSubject(null)).toBe('')
  })
})

describe('createTokenRefresher', () => {
  it('续期后两张都存（后端会轮换，只存 access 的话下一次续期必失败）', async () => {
    const storage = { pair: { accessToken: jwt('1', 1), refreshToken: 'R1-1' } }
    const t = tab(storage, storage.pair, rotatingServer('1'))
    await t.refresh(jwt('1', 1))
    expect(t.mem().refreshToken).toBe('R2-1')
    // 第二次续期用的是轮换后的那张，能成功
    await t.refresh(t.mem().accessToken)
    expect(t.calls).toEqual(['R1-1', 'R2-1'])
    expect(storage.pair.refreshToken).toBe('R3-1')
  })

  it('几个请求同时撞上 401：只续一次，其余等同一个结果', async () => {
    const stale = jwt('1', 1)
    const storage = { pair: { accessToken: stale, refreshToken: 'R1-1' } }
    const t = tab(storage, storage.pair, rotatingServer('1'))
    const results = await Promise.all([t.refresh(stale), t.refresh(stale), t.refresh(stale)])
    expect(t.calls).toEqual(['R1-1'])
    expect(new Set(results).size).toBe(1)
  })

  it('失败请求带的 access 已经不是当前的（刚有别的请求续过）：直接用当前的，不再续', async () => {
    const storage = { pair: { accessToken: jwt('1', 1), refreshToken: 'R1-1' } }
    const t = tab(storage, storage.pair, rotatingServer('1'))
    const fresh = await t.refresh(jwt('1', 1))
    expect(await t.refresh(jwt('1', 1))).toBe(fresh)
    expect(t.calls).toHaveLength(1)
  })

  it('别的标签页刚续过：本页拿着旧 refresh 也不掉线，采用存储里那一对', async () => {
    const server = rotatingServer('1')
    const first = { accessToken: jwt('1', 1), refreshToken: 'R1-1' }
    const storage = { pair: { ...first } }
    const a = tab(storage, first, server)
    const b = tab(storage, first, server)
    await a.refresh(first.accessToken) // A 先续：R1 作废，存储里是 R2
    const got = await b.refresh(first.accessToken) // B 手里还是 R1
    expect(got).toBe(storage.pair.accessToken)
    expect(b.calls).toEqual([]) // 没有拿已作废的 R1 去撞服务端
    expect(b.mem().refreshToken).toBe('R2-1')
  })

  it('两个标签页同时续：输的那个续期失败后改用赢的那个写进存储的一对', async () => {
    const server = rotatingServer('1')
    const first = { accessToken: jwt('1', 1), refreshToken: 'R1-1' }
    const storage = { pair: { ...first } }
    const a = tab(storage, first, server)
    const b = tab(storage, first, server)
    const [ra, rb] = await Promise.all([a.refresh(first.accessToken), b.refresh(first.accessToken)])
    expect(ra).toBe(rb)
    expect(a.mem().refreshToken).toBe(b.mem().refreshToken)
  })

  it('存储里是**别的账号**（另一个标签页换了号）：不采用，照常用自己的续', async () => {
    const mine = { accessToken: jwt('1', 1), refreshToken: 'R1-1' }
    const storage = { pair: { accessToken: jwt('2', 9), refreshToken: 'R9-2' } }
    const t = tab(storage, mine, rotatingServer('1'))
    const got = await t.refresh(mine.accessToken)
    expect(tokenSubject(got)).toBe('user:1')
    expect(t.calls).toEqual(['R1-1'])
  })

  it('refresh 真的失效了（被撤销 / 过期）：抛出，由调用方清会话', async () => {
    const storage = { pair: { accessToken: jwt('1', 1), refreshToken: 'R-dead' } }
    const t = tab(storage, storage.pair, rotatingServer('1'))
    await expect(t.refresh(jwt('1', 1))).rejects.toThrow('凭证已被撤销')
    // 失败之后可以再试（不会卡在一个失败的进行中请求上）
    await expect(t.refresh(jwt('1', 1))).rejects.toThrow()
    expect(t.calls).toEqual(['R-dead', 'R-dead'])
  })

  it('没有 refresh：直接抛出', async () => {
    const t = tab({ pair: { accessToken: null, refreshToken: null } }, { accessToken: null, refreshToken: null }, rotatingServer('1'))
    await expect(t.refresh()).rejects.toThrow('refresh')
  })
})

describe('isDefinitiveRefreshFailure', () => {
  it('只有续期接口明确回 4xx 才是定论', () => {
    expect(isDefinitiveRefreshFailure({ status: 401 })).toBe(true)
    expect(isDefinitiveRefreshFailure({ statusCode: 403 })).toBe(true) // 账号被封
    expect(isDefinitiveRefreshFailure({ response: { status: 401 } })).toBe(true) // ofetch 的 FetchError
    expect(isDefinitiveRefreshFailure({ status: 502 })).toBe(false)
    expect(isDefinitiveRefreshFailure(new TypeError('Failed to fetch'))).toBe(false) // 断网没有状态码
    expect(isDefinitiveRefreshFailure(null)).toBe(false)
  })

  it('续期器自己抛的：没有 refresh 是定论；200 却缺 token 不是（服务端多半已轮换，宽限期内再续能接上）', async () => {
    const none = tab({ pair: { accessToken: null, refreshToken: null } }, { accessToken: null, refreshToken: null }, rotatingServer('1'))
    expect(isDefinitiveRefreshFailure(await none.refresh().catch((e: unknown) => e))).toBe(true)
    const storage = { pair: { accessToken: jwt('1', 1), refreshToken: 'R1-1' } }
    const empty = tab(storage, storage.pair, async () => ({ accessToken: '', refreshToken: '' }))
    expect(isDefinitiveRefreshFailure(await empty.refresh(jwt('1', 1)).catch((e: unknown) => e))).toBe(false)
  })
})

describe('sendWithRefresh', () => {
  /** 只认 `valid` 这张 access 的假接口。 */
  function endpoint(valid: string) {
    const sent: (string | null)[] = []
    return { sent, send: async (t: string | null) => { sent.push(t); return { status: t === valid ? 200 : 401 } } }
  }

  it('不是 401：原样返回，不续期', async () => {
    const api = endpoint('A1')
    const refreshes: (string | null)[] = []
    const res = await sendWithRefresh('A1', api.send, async (s) => { refreshes.push(s); return 'A2' })
    expect(res.status).toBe(200)
    expect(refreshes).toEqual([])
  })

  it('401：带着发出时那张去续，用续好的重发一次', async () => {
    const api = endpoint('A2')
    const refreshes: (string | null)[] = []
    const res = await sendWithRefresh('A1', api.send, async (s) => { refreshes.push(s); return 'A2' })
    expect(res.status).toBe(200)
    expect(refreshes).toEqual(['A1'])
    expect(api.sent).toEqual(['A1', 'A2'])
  })

  it('续期被明确拒绝（4xx）：交回那次 401，不重发（由调用方决定要不要清会话）', async () => {
    const api = endpoint('A2')
    const res = await sendWithRefresh('A1', api.send, async () => { throw Object.assign(new Error('凭证已被撤销'), { status: 401 }) })
    expect(res.status).toBe(401)
    expect(api.sent).toEqual(['A1'])
  })

  it('续期没有定论（断网 / 5xx）：把错误抛出去，**不**当成 401（否则一次网络抖动就被登出）', async () => {
    const api = endpoint('A2')
    await expect(sendWithRefresh('A1', api.send, async () => { throw new TypeError('Failed to fetch') })).rejects.toThrow('Failed to fetch')
    await expect(sendWithRefresh('A1', api.send, async () => { throw Object.assign(new Error('bad gateway'), { status: 502 }) })).rejects.toThrow('bad gateway')
    expect(api.sent).toEqual(['A1', 'A1'])
  })

  it('续好之后仍是 401：交回它，只重发一次', async () => {
    const api = endpoint('never')
    let n = 0
    const res = await sendWithRefresh('A1', api.send, async () => `A${++n + 1}`)
    expect(res.status).toBe(401)
    expect(api.sent).toEqual(['A1', 'A2'])
    expect(n).toBe(1)
  })
})
