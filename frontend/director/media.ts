/**
 * 引擎取受保护媒体的入口（DRD-40，dev/03 §6.6）：编辑器在 setup 里调一次，把返回的函数注入引擎（`deps.fetchMediaBlob`）。
 *
 * ⚠️ 只能先取成 blob：受保护地址直接交给 TextureLoader 会 401（`<img>` 与纹理加载都不带鉴权头）；
 *    跨域图还会污染 canvas，之后截图时 `toBlob` 抛 SecurityError。
 * ⚠️ 请求头走 `authorizedMediaRequest` 这个唯一来源（带 `X-Workspace-Id`，漏了回 400）；access 过期时续期后重试一次。
 *    这里不用 `useAuthenticatedMedia`：它会注册 onScopeDispose，按次调用是错的（同 stores/generation.ts 的做法）。
 * ⚠️ 必须在组件 setup 里调用：store 与运行时配置在这时取到，之后的异步调用不再依赖 Nuxt 上下文。
 */
import { useRuntimeConfig } from "#imports";
import { useAuthStore } from "~/stores/auth";
import { useWorkspaceStore } from "~/stores/workspace";
import { authorizedMediaRequest } from "~/utils/authorizedMedia";

export function createDirectorMediaFetcher(): (mediaId: string) => Promise<Blob> {
  const auth = useAuthStore();
  const workspace = useWorkspaceStore();
  const apiBase = String(useRuntimeConfig().public.apiBase);
  return async (mediaId) => {
    const response = await auth.withTokenRefresh((token) => {
      const init = authorizedMediaRequest(`/media/${encodeURIComponent(mediaId)}`, token, apiBase, workspace.currentId);
      return fetch(init.url, { headers: init.headers });
    });
    if (!response.ok) throw new Error(`媒体 ${mediaId} 加载失败（HTTP ${response.status}）`);
    return response.blob();
  };
}
