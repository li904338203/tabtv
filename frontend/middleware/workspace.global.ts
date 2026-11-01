/**
 * 进业务页之前先把工作区取回来（P15-01）。
 *
 * ⚠️ 不做这一步的话，首页会在工作区就绪**之前**发出第一个请求，
 * 拿到 400「缺少 X-Workspace-Id 请求头」—— 而那看起来像参数写错，
 * 不像"还没准备好"。
 */
export default defineNuxtRouteMiddleware(async (to) => {
  if (import.meta.server) return
  const auth = useAuthStore()
  if (!auth.accessToken) return
  if (to.path.startsWith('/login') || to.path.startsWith('/register')) return
  await useWorkspaceStore().load()
})
