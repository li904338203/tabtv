import { absoluteMediaUrl, authorizedMediaRequest } from "~/utils/authorizedMedia";

/**
 * 加载需要鉴权的媒体（生成产出、参考图等）。
 *
 * <p>产出地址形如 `/api/v1/generations/{id}/outputs/0`，接口要 Bearer token，
 * 而 `<img src>` 不会带 Authorization 头 —— 直接引用一律 401。所以必须先 fetch 成 blob，
 * 再用 object URL 交给 img。
 *
 * <p>抽成 composable 是因为这段逻辑此前在 works.vue、GroupImageNodePanel、stores/generation
 * 各抄了一份，第四处再抄就该出事了：最容易漏的是 revokeObjectURL —— 不释放的话每次刷新列表
 * 都会在内存里留下一份完整图片，翻几页就是几百 MB。
 */
export function useAuthenticatedMedia() {
  const auth = useAuthStore();
  const workspace = useWorkspaceStore();
  const config = useRuntimeConfig();
  const urls = ref<Record<string, string>>({});
  /**
   * 作用域已销毁（组件卸载、弹窗关闭）。⚠️ 取图是异步的：卸载之后才回来的那一张若照常 createObjectURL，
   * 它已经错过了 release，永远不会被 revoke（MKT-T-38：弹窗开着就关掉时必现）。
   */
  let disposed = false;

  function absolute(url: string) {
    return absoluteMediaUrl(url, String(config.public.apiBase));
  }

  /**
   * 取受保护媒体的请求参数 —— **全站唯一来源**。
   *
   * ⚠️ 除了 Authorization 还必须带 **`X-Workspace-Id`**：漏了的话
   *    `/api/v1/media/{id}` 回 **400「缺少 X-Workspace-Id 请求头」**，
   *    而界面上只是一张不显示的图。
   */
  function request(url: string, token: string | null = auth.accessToken ?? null) {
    return authorizedMediaRequest(
      url,
      token,
      String(config.public.apiBase),
      workspace.currentId
    );
  }

  /**
   * 原生 fetch 版：调用方自己看 `response.ok`（循环里要 `continue` 的场景用它）。
   * ⚠️ 走这条也必须经过 `request()` —— 别在调用处自己拼 headers。
   * ⚠️ access 过期（401）时续期后重试一次：这条路不经过 useApi，不在这里续的话
   *    15 分钟后画布上的图整片变成裂图（见 auth store 的 `withTokenRefresh`）。
   */
  async function fetchMedia(url: string): Promise<Response> {
    return await auth.withTokenRefresh((token) => {
      const init = request(url, token);
      return fetch(init.url, { headers: init.headers });
    });
  }

  async function blob(url: string) {
    const response = await fetchMedia(url);
    if (!response.ok) throw new Error(`媒体加载失败（HTTP ${response.status}）`);
    return await response.blob();
  }

  /** 释放已持有的 object URL。换页、重新加载与组件卸载都必须调。 */
  function release() {
    Object.values(urls.value).forEach(value => URL.revokeObjectURL(value));
    urls.value = {};
  }

  /**
   * 批量加载，结果按 key 存放。
   *
   * <p>用 allSettled 而不是 all：一张图 404 或过期不该让整屏封面都不显示，
   * 失败的那一项自然缺席，调用方退回占位即可。
   */
  async function loadAll(items: { key: string; url?: string | null }[]) {
    release();
    await Promise.allSettled(items.map(async (item) => {
      if (!item.url) return;
      const data = await blob(item.url);
      if (!disposed) urls.value[item.key] = URL.createObjectURL(data);
    }));
  }

  /**
   * 追加加载，<b>不</b>释放已有的。
   *
   * <p>下拉分页专用：{@link loadAll} 每次先 release，用在滚动加载上会把已经滚过的封面
   * 全部释放掉，越滚越空。代价是 blob 只增不减 —— 所以列表场景必须请求缩略图
   * （产出接口的 ?w= 参数），原图单张 1.7MB，几百张就能把标签页撑爆。
   */
  async function appendAll(items: { key: string; url?: string | null }[]) {
    await Promise.allSettled(items.map(async (item) => {
      // 已经有的不重复拉：翻页边界上重复项会白白多占一份内存
      if (!item.url || urls.value[item.key]) return;
      const data = await blob(item.url);
      // 并发取同一张时只留一份（另一份会占着内存且没人释放）；卸载后到的不再建。
      if (!disposed && !urls.value[item.key]) urls.value[item.key] = URL.createObjectURL(data);
    }));
  }

  onScopeDispose(() => {
    disposed = true;
    release();
  });

  return { urls, loadAll, appendAll, release, blob, absolute, request, fetchMedia };
}
