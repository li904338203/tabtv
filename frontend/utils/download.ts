/**
 * 把一个 Blob 存成本地文件。
 *
 * ⚠️ 受保护的产出**不能**让 `<a>` 的 href 直接指向 API 端点下载：`<a>` 与 `<img>` 一样
 *    不带 Authorization / X-Workspace-Id 头，一律 401/400（GAP-18）。先 fetch 成 blob，再交给这里。
 * ⚠️ **不能 click 完立刻 revoke**：部分浏览器是在 click 之后**异步**去取那个 object URL 的，
 *    同步撤销会下出一个 0 字节的文件 —— 而下载本身显示「已完成」。
 */
export function saveBlob(blob: Blob, filename: string, delayMs = 1000): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), delayMs);
}

const EXTENSIONS: Record<string, string> = {
  "image/png": "png",
  "image/jpeg": "jpg",
  "image/webp": "webp",
  "image/gif": "gif",
  "video/mp4": "mp4",
  "video/webm": "webm",
  "video/quicktime": "mov",
  "audio/mpeg": "mp3",
  "audio/mp3": "mp3",
  "audio/wav": "wav",
  "audio/x-wav": "wav",
  "audio/ogg": "ogg",
  "text/plain": "txt",
  "application/json": "json"
};

/**
 * 按 Content-Type 取扩展名。
 *
 * ⚠️ 不带扩展名的话（老系统就是这样：`项目名-ID`），下下来的文件双击打不开 ——
 *    系统不知道该用什么程序，用户以为下坏了。认不出的类型给 `bin`，不猜。
 */
export function extensionFor(contentType: string | null | undefined): string {
  const type = (contentType ?? "").split(";", 1)[0]!.trim().toLowerCase();
  return EXTENSIONS[type] ?? "bin";
}

/**
 * 文件名里去掉系统不允许的字符。项目名是用户输入的，里面完全可能有 `/`、`:`、`?`。
 * 为空时用兜底名，不产生一个只有扩展名的文件。
 */
export function safeFilename(base: string, fallback = "AITv作品"): string {
  const FORBIDDEN = '\\/:*?"<>|';
  const mapped = Array.from(base, ch => (ch.charCodeAt(0) < 0x20 || FORBIDDEN.includes(ch) ? "_" : ch)).join("");
  // 连续的非法字符只留一个下划线；按字符截断（Array.from），中文不会截出半个字
  const cleaned = Array.from(mapped.replace(/_+/g, "_").trim()).slice(0, 80).join("");
  return cleaned || fallback;
}
