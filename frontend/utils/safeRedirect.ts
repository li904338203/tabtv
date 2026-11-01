/**
 * 登录回跳地址（AGT-43）：只接受**站内相对路径**，其余一律回到兜底地址。
 *
 * ⚠️ 不做这一步就是开放重定向：`/login?redirect=https://evil.com` 让人在我们的登录页输完密码后
 *    被带去一个长得一样的钓鱼站。要挡的不只是 `http(s)://`：
 *    - `//evil.com`（协议相对地址，浏览器当成外站）；
 *    - `/\evil.com`、`\\evil.com`（部分浏览器把反斜杠当斜杠）；
 *    - 带控制字符 / 换行的（`/%0d%0a…` 解码后），以及 `javascript:` 之类的伪协议；
 *    - 回跳到登录页自己（会原地打转）。
 * 判定落在 URL 解析上：拼到一个假的源上解析，解析后源变了就不是站内地址。
 */
const PROBE_ORIGIN = "http://seeaitv.invalid";

export function safeRedirect(raw: unknown, fallback = "/"): string {
  const value = Array.isArray(raw) ? raw[0] : raw;
  if (typeof value !== "string") return fallback;
  const target = value.trim();
  if (!target.startsWith("/") || target.startsWith("//")) return fallback;
  // 反斜杠与控制字符：浏览器各自有纠错规则，宁可一律不收。
  // eslint-disable-next-line no-control-regex
  if (/[\\\u0000-\u001f\u007f]/.test(target)) return fallback;
  let url: URL;
  try {
    url = new URL(target, PROBE_ORIGIN);
  } catch {
    return fallback;
  }
  if (url.origin !== PROBE_ORIGIN) return fallback;
  if (url.pathname === "/login" || url.pathname.startsWith("/login/")) return fallback;
  return `${url.pathname}${url.search}${url.hash}`;
}

/** 去登录页的地址，带上回跳（回跳本身也过一遍 `safeRedirect`，免得把坏地址一路带过去）。 */
export function loginPath(redirect: string): string {
  const target = safeRedirect(redirect, "");
  return target && target !== "/" ? `/login?redirect=${encodeURIComponent(target)}` : "/login";
}
