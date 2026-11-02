import { onHostEvent } from "~/utils/desktopBridge";

/**
 * 实时事件的唯一入口：多 topic 引用计数订阅、按事件 `type` 分发（dev/01 TTD-3，TT-T-11）。
 *
 * 来源系统这里是一条 WebSocket（`/ws/events`：握手、首帧鉴权、续期、退避重连、subscribe 帧）。
 * tabtv 是桌面应用：后端在同一个进程里，事件经 Wails 的事件通道送来，每条是 `{topic, payload}`。
 * 对外接口（state / subscribe / on / onReconnected / onDisconnected / connect / disconnect）保持不变，
 * 8 个订阅方一行不改。
 *
 * ⚠️ 事件仍只是「该去对账了」的信号（GO-26）：这里**不解析事件字段**，原样交给订阅方。
 * ⚠️ 只分发**本页订了的 topic**：后端把全部事件都推过来（单窗口，没有「投给谁」的问题），
 *    不过滤的话别的项目的画布操作会落到当前画布上。
 * ⚠️ 没有宿主（浏览器直接打开开发服务器）时状态为 offline，订阅方照常靠轮询。
 */
export type RealtimeState = "offline" | "connecting" | "online";
export type RealtimeFrame = { type?: unknown } & Record<string, unknown>;
type FrameHandler = (frame: RealtimeFrame) => void;

export const useRealtimeStore = defineStore("realtime", () => {
  // topic → 引用数。
  const refCount = new Map<string, number>();
  const handlers = new Map<string, Set<FrameHandler>>();
  const reconnectedHandlers = new Set<() => void>();
  const disconnectedHandlers = new Set<() => void>();

  const attached = ref(false);
  // 已订阅的 topic（ref 包 Set 是响应式集合，isSubscribed 能被 computed 追踪）。
  const accepted = ref(new Set<string>());
  let detach: (() => void) | undefined;

  const state = computed<RealtimeState>(() => (attached.value && accepted.value.size > 0 ? "online" : "offline"));

  function isSubscribed(topic: string): boolean {
    return attached.value && accepted.value.has(topic);
  }

  function activeTopics(): string[] {
    return [...refCount.keys()];
  }

  /**
   * 订阅一个 topic，返回释放函数。多处订同一个 topic 共用一份，**最后一处释放才真正退订**。
   * ⚠️ 释放函数只生效一次：组件卸载与 store 关闭可能各调一次，重复减计数会把别人的订阅减没。
   */
  function subscribe(topic: string): () => void {
    refCount.set(topic, (refCount.get(topic) ?? 0) + 1);
    accepted.value.add(topic);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      const left = (refCount.get(topic) ?? 0) - 1;
      if (left > 0) {
        refCount.set(topic, left);
        return;
      }
      refCount.delete(topic);
      accepted.value.delete(topic);
    };
  }

  /** 按事件 `type` 注册处理函数，返回解除函数。 */
  function on(type: string, handler: FrameHandler): () => void {
    let set = handlers.get(type);
    if (!set) handlers.set(type, (set = new Set()));
    set.add(handler);
    return () => {
      set.delete(handler);
      if (set.size === 0 && handlers.get(type) === set) handlers.delete(type);
    };
  }

  /** 连上之后触发一次：订阅方在这里用 HTTP 对账（订上之前落定的事，推送已经错过了）。 */
  function onReconnected(handler: () => void): () => void {
    reconnectedHandlers.add(handler);
    return () => reconnectedHandlers.delete(handler);
  }

  /** 连接意外断开时触发。桌面版事件通道与进程同生共死，不会单独断开；保留接口给订阅方。 */
  function onDisconnected(handler: () => void): () => void {
    disconnectedHandlers.add(handler);
    return () => disconnectedHandlers.delete(handler);
  }

  function emit(list: Iterable<() => void>) {
    for (const handler of [...list]) {
      try {
        handler();
      } catch {
        // 一个订阅方出错不影响其余订阅方。
      }
    }
  }

  function dispatch(topic: string, frame: RealtimeFrame) {
    if (!refCount.has(topic)) return;
    const type = typeof frame.type === "string" ? frame.type : "";
    const set = handlers.get(type);
    if (!set) return;
    for (const handler of [...set]) {
      try {
        handler(frame);
      } catch {
        // 事件只是对账信号，单个处理函数抛错不该打断分发。
      }
    }
  }

  function connect() {
    if (attached.value) return;
    const off = onHostEvent((topic, payload) => {
      if (payload && typeof payload === "object") dispatch(topic, payload as RealtimeFrame);
    });
    if (!off) return; // 没有宿主：保持 offline，订阅方靠轮询
    detach = off;
    attached.value = true;
    // 与来源系统「先订阅、再对账」的约定一致：接上之后让订阅方对一次账。
    queueMicrotask(() => emit(reconnectedHandlers));
  }

  function disconnect() {
    detach?.();
    detach = undefined;
    attached.value = false;
  }

  return {
    state,
    isSubscribed,
    activeTopics,
    subscribe,
    on,
    onReconnected,
    onDisconnected,
    connect,
    disconnect
  };
});
