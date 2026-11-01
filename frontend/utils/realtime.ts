/**
 * 实时通道（`/ws/events`）的协议细节 —— 抽成纯函数以便单测（协议见 `api/events/README.md`）。
 *
 * ⚠️ 移植过来的老前端照老协议发 `{type:"subscribe", projectId}`，而新后端只认 `topics` 数组：
 *    这一帧被当成「什么都没订」，服务端照样回 `subscribe.ok` —— 界面上显示「在线」，
 *    却一条事件都收不到，任务进度全靠每秒一次的轮询，轮询耗尽之后长任务就只能等手动刷新。
 *    **没有任何报错**。
 * ⚠️ 生成事件按**工作区**投递（`workspace:{id}`：团队里别的成员也该看得到进度），
 *    不是按项目 —— 订 `project:{id}` 同样一条都收不到。
 */

/**
 * 实时通道的地址。
 *
 * - 显式配了完整地址（`NUXT_PUBLIC_WS_URL`）就用它；
 * - 否则**跟着页面的主机名走**：开发时补上 API 端口（页面 :3000、API :8180），
 *   生产构建不补（页面与 API 经反向代理同域）。
 *
 * ⚠️ 此前写死 `ws://localhost:8180`：从别的设备（手机、同事的电脑）用局域网地址打开页面时，
 *    localhost 指的是**那台设备自己** —— 连不上，而界面只显示「离线」。
 * ⚠️ 不能走开发代理：nitro 的 devProxy 不转发 WebSocket 升级（升级请求交给了 Nuxt 自己的 worker）。
 */
export function realtimeUrl(
  configured: string,
  apiPort: string,
  page: { protocol: string; hostname: string; host: string }
): string {
  if (configured) return configured;
  const scheme = page.protocol === "https:" ? "wss" : "ws";
  // hostname 对 IPv6 自带方括号（`[::1]`），直接拼端口即可
  const authority = apiPort ? `${page.hostname}:${apiPort}` : page.host;
  return `${scheme}://${authority}/ws/events`;
}

export function workspaceTopic(workspaceId: string): string {
  return `workspace:${workspaceId}`;
}

/**
 * 项目 topic（`project:{id}`）。⚠️ 生成事件**不走**这里（按工作区投递，见文件头）；
 * 它留给画布协作类事件（`canvas.ops_applied` 等，AGT-11 接入）。
 */
export function projectTopic(projectId: string): string {
  return `project:${projectId}`;
}

/**
 * 订阅帧。收单个 topic（旧签名，兼容既有调用）或一组 topic —— 重连后要**一帧**重发全部仍被引用的 topic。
 */
export function subscribeFrame(topics: string | readonly string[]) {
  return { type: "subscribe", topics: typeof topics === "string" ? [topics] : [...topics] };
}

/** 退订帧（服务端 `unsubscribe` 收 topics 数组，不回应答）。 */
export function unsubscribeFrame(topics: string | readonly string[]) {
  return { type: "unsubscribe", topics: typeof topics === "string" ? [topics] : [...topics] };
}

type Frame = { type?: unknown; topics?: unknown; generationId?: unknown; projectId?: unknown };

function topicsOf(frame: unknown): string[] {
  const topics = (frame as Frame | null)?.topics;
  return Array.isArray(topics) ? topics.filter((t): t is string => typeof t === "string") : [];
}

/**
 * `subscribe.ok` 里**真的订上了**的 topic。
 * ⚠️ 只取 `topics`、不取 `rejected`：被拒的服务端照样回 `subscribe.ok`（CD-73），把它算进去界面就显示「在线」。
 */
export function acceptedTopics(frame: unknown): string[] {
  return (frame as Frame | null)?.type === "subscribe.ok" ? topicsOf(frame) : [];
}

/** `subscription.revoked` 撤销了哪些 topic。 */
export function revokedTopics(frame: unknown): string[] {
  return (frame as Frame | null)?.type === "subscription.revoked" ? topicsOf(frame) : [];
}

function hasTopic(frame: unknown, topic: string): boolean {
  const topics = (frame as Frame | null)?.topics;
  return Array.isArray(topics) && topics.includes(topic);
}

/**
 * `subscribe.ok` 里这个 topic 是不是真的订上了。
 * ⚠️ 被拒的放在 `rejected` 里，服务端照样回 `subscribe.ok` —— 只看帧类型就会把「被拒」当成「在线」。
 */
export function subscribed(frame: unknown, topic: string): boolean {
  return (frame as Frame | null)?.type === "subscribe.ok" && hasTopic(frame, topic);
}

/**
 * 断线后隔多久重连（failures = 连续第几次没连上，从 1 起）。
 *
 * ⚠️ 连不上时要**退避**：固定 1.5 秒重连的话，握手被拒（来源没放行）或服务端没起来时，
 *    一个开着的画布页每分钟 40 次握手失败，而界面上只是一个「离线」。订上之后计数清零。
 */
export function reconnectDelay(failures: number): number {
  return Math.min(30_000, 1500 * 2 ** Math.max(0, failures - 1));
}

/** `subscription.revoked` 撤销了这个 topic（被移出工作区等）：之后不会再有推送，退回轮询。 */
export function revoked(frame: unknown, topic: string): boolean {
  return (frame as Frame | null)?.type === "subscription.revoked" && hasTopic(frame, topic);
}

/**
 * 一条生成事件要不要处理、落到哪个节点；不处理返回 null。
 *
 * ⚠️ 事件只当「该去对账了」的信号，**不拿事件里的字段当结果**：产物要另取（文本产物的正文只在产物端点上），
 *    原因与状态以库里为准 —— 事件不是事实来源（GO-26），会乱序、会重复、会丢。
 * ⚠️ 只处理本页**正在跟踪**的任务：
 *  - 已经落定的不再跟踪 —— 晚到的旧事件（乱序）会把一个已完成的节点打回「运行中」；
 *  - 同一工作区里别人 / 别的标签页发起的不处理 —— 每个开着的标签页都去写结果、存画布的话，彼此 409。
 */
export function eventTarget(
  frame: unknown,
  projectId: string,
  tracked: ReadonlyMap<string, string>
): { generationId: string; nodeId: string } | null {
  const f = frame as Frame | null;
  if (f?.type !== "generation.update" || typeof f.generationId !== "string") return null;
  if (f.projectId !== projectId) return null;
  const nodeId = tracked.get(f.generationId);
  return nodeId ? { generationId: f.generationId, nodeId } : null;
}
