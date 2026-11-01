/**
 * 工作区 —— **新后端独有的概念**（P15-01 前端移植）。
 *
 * 老系统按**用户**分数据，新系统按**工作区**分租户：几乎每个业务端点都要
 * `X-Workspace-Id`，而且服务端两条都校验（调用者是成员 且 资源归属一致）。
 *
 * ⚠️ 漏发这个头的表现是 **400「缺少 X-Workspace-Id 请求头」** —— 不是 401，
 * 所以看起来像参数写错，而不是像没登录。
 *
 * ⚠️ 必须在**页面发第一个业务请求之前**就绪，否则首页会先撞一次 400。
 * 因此由 `middleware/workspace.global.ts` 在路由前拉取。
 */
export type Workspace = {
  id: string
  name: string
  kind: 'personal' | 'team'
  role: 'owner' | 'admin' | 'editor' | 'viewer'
  payerAccountId: string
}

const STORAGE_KEY = 'seeaitv.workspace'

export const useWorkspaceStore = defineStore('workspace', () => {
  const list = ref<Workspace[]>([])
  const currentId = ref<string>('')
  const loaded = ref(false)

  const current = computed(() => list.value.find(w => w.id === currentId.value) ?? null)

  function select(id: string) {
    if (!list.value.some(w => w.id === id)) return
    currentId.value = id
    if (import.meta.client) localStorage.setItem(STORAGE_KEY, id)
  }

  function setList(items: Workspace[]) {
    list.value = items
    // 恢复上次选择；失效（被移除、被删）则回落到个人空间。
    // ⚠️ 不能停在一个已经没有权限的工作区上 —— 那会让每个接口都报 403。
    const saved = import.meta.client ? localStorage.getItem(STORAGE_KEY) : null
    const valid = saved && items.some(w => w.id === saved)
    currentId.value = valid
      ? saved!
      : (items.find(w => w.kind === 'personal')?.id ?? items[0]?.id ?? '')
    if (import.meta.client && currentId.value) {
      localStorage.setItem(STORAGE_KEY, currentId.value)
    }
  }

  /**
   * 拉取工作区列表。
   *
   * ⚠️ 这里**直接用 $fetch 而不是 useApi()**：`useApi` 会去读本 store 的
   * `currentId` 来拼工作区头，而此刻它正是空的 —— 用 useApi 会绕成循环依赖，
   * 且"取工作区列表"本来就不属于任何一个工作区。
   */
  // 单飞：页面一次发出好几个请求时都会来等工作区（见 useApi），各拉一次就是好几个重复请求。
  let loading: Promise<void> | null = null
  // 每 clear() 一次进一代：拉取途中换了账号的话，回来的是上一个账号的列表，必须丢掉。
  let epoch = 0

  async function load() {
    if (loaded.value) return
    if (loading) return loading
    const auth = useAuthStore()
    if (!auth.accessToken) return
    const config = useRuntimeConfig()
    const mine = epoch
    loading = (async () => {
      try {
        // ⚠️ access 过期时要续期再取：这里是页面加载的第一个请求，取不到的话工作区为空，
        //    之后每个业务请求都缺工作区头（400）—— 过期后刷新一下页面就整页报错。
        const response = await auth.withTokenRefresh(token => $fetch.raw<Workspace[]>(`${config.public.apiBase}/workspaces`, {
          headers: token ? { Authorization: `Bearer ${token}` } : undefined,
          ignoreResponseError: true,
        }))
        if (mine !== epoch || !response.ok) return
        const items = response._data
        setList(Array.isArray(items) ? items : [])
        loaded.value = true
      } catch {
        // ⚠️ 拉不到**不要踢去登录页**：可能只是网断了。下一个请求会再来取（见 useApi）。
        // 让页面自己显示它的错误态（401 由 useApi 统一处置）。
      }
    })().finally(() => {
      if (mine === epoch) loading = null
    })
    return loading
  }

  function clear() {
    // ⚠️ 进行中的那次拉取一起作废：不清的话，换账号之后的 load() 会直接复用它、拿到上一个账号的工作区。
    epoch += 1
    loading = null
    list.value = []
    currentId.value = ''
    loaded.value = false
    if (import.meta.client) localStorage.removeItem(STORAGE_KEY)
  }

  return { list, current, currentId, loaded, select, setList, load, clear }
})
