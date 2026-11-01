/**
 * 宿主启动失败（数据库拉不起来、迁移失败等）时直接进错误页（TT-F-02）。
 * 不这么做的话页面会照常渲染，然后每个请求都拿到 503 —— 用户看到的是一屏莫名其妙的加载失败。
 */
export default defineNuxtPlugin(async () => {
  const host = hostApp();
  if (!host) return;
  const failure = await host.GetStartupError().catch(() => null);
  if (failure) {
    await navigateTo(STARTUP_ERROR_PATH, { replace: true });
  }
});
