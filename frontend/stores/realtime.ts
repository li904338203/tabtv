import { isDefinitiveRefreshFailure } from "@tabtv/api-client";
import { acceptedTopics, realtimeUrl, reconnectDelay, revokedTopics, subscribeFrame, unsubscribeFrame } from "~/utils/realtime";

/**
 * 实时通道（`/ws/events`）的**唯一连接**：握手、续期、退避重连、多 topic 引用计数订阅、按事件 `type` 分发。
 *
 * 此前连接长在 `stores/generation.ts` 里，只订得了一个 `workspace:{id}`、只认 `generation.update`；
 * 画布协作（`project:{id}`）要再订一个 topic 时，只能再开一条连接 —— 两条连接各自续期、各自退避，
 * 一个被拒另一个照样「在线」。抽出来之后，各业务 store 只声明「我要哪个 topic、关心哪种事件」。
 *
 * ⚠️ 事件只是「该去对账了」的信号（GO-26）：这里**不解析事件字段**，原样交给订阅方；
 *    重连之后订阅方还要再对一次账（`onReconnected`）—— 断线期间错过的推送不会补发。
 */
export type RealtimeState = "offline" | "connecting" | "online";
export type RealtimeFrame = { type?: unknown } & Record<string, unknown>;
type FrameHandler = (frame: RealtimeFrame) => void;

// 由 store 自己消化、不对外分发的帧。
const INTERNAL = new Set(["auth", "auth.ok", "auth.error", "subscribe.ok", "subscription.revoked", "pong"]);

export const useRealtimeStore = defineStore("realtime", () => {
  const auth = useAuthStore();
  const config = useRuntimeConfig();

  let socket: WebSocket | undefined;
  let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  // 连续几次没订上：决定下一次隔多久重连（见 reconnectDelay），订上即清零。
  let failures = 0;
  let intentionalClose = false;
  // 本次连接鉴权通过之后、第一帧 subscribe.ok 到达之前为 true：那一帧到了才触发 onReconnected。
  let resyncPending = false;

  // topic → 引用数。0 → 1 发 subscribe、1 → 0 发 unsubscribe。
  const refCount = new Map<string, number>();
  const handlers = new Map<string, Set<FrameHandler>>();
  const reconnectedHandlers = new Set<() => void>();
  const disconnectedHandlers = new Set<() => void>();

  const socketOpen = ref(false); // 有一条正在连 / 已连上的 socket
  const authed = ref(false);
  const awaitingAck = ref(0); // 发出去、还没等到 subscribe.ok 的订阅帧数
  // 服务端真的订上了的 topic（ref 包 Set 是响应式集合，isSubscribed 能被 computed 追踪）。
  const accepted = ref(new Set<string>());

  /**
   * ⚠️ `online` = 已鉴权**且至少有一个 topic 真的订上了**。被拒的 topic 服务端照样回 `subscribe.ok`（CD-73），
   *    只看帧类型的话「订了个寂寞」也显示在线。单个 topic 是否在线用 `isSubscribed`。
   */
  const state = computed<RealtimeState>(() => {
    if (!socketOpen.value) return "offline";
    if (!authed.value) return "connecting";
    if (accepted.value.size > 0) return "online";
    return awaitingAck.value > 0 ? "connecting" : "offline";
  });

  function isSubscribed(topic: string): boolean {
    return accepted.value.has(topic);
  }

  /** 当前仍被引用的 topic（决定没人用时要不要断开）。 */
  function activeTopics(): string[] {
    return [...refCount.keys()];
  }

  function send(frame: unknown) {
    if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(frame));
  }

  /**
   * 订阅一个 topic，返回释放函数。
   *
   * 多处订同一个 topic 只发一次 subscribe，**最后一处释放才发 unsubscribe** ——
   * 否则画布页退订 `project:{id}` 时会把别处还在用的同名订阅一起断掉。
   * ⚠️ 释放函数只生效一次：组件卸载与 store 关闭可能各调一次，重复减计数会把别人的订阅减没。
   */
  function subscribe(topic: string): () => void {
    const count = refCount.get(topic) ?? 0;
    refCount.set(topic, count + 1);
    // 还没鉴权就先记账：auth.ok 时会一帧发出全部仍被引用的 topic。
    if (count === 0 && authed.value) {
      awaitingAck.value += 1;
      send(subscribeFrame([topic]));
    }
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
      if (authed.value) send(unsubscribeFrame([topic]));
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

  /**
   * (重)连接建立、全部 topic 重发之后，**第一帧 `subscribe.ok` 到达时**触发（首次连接也算）。
   * 订阅方在这里用 HTTP 对账：订上之前那段时间落定的事，推送已经错过了（协议约定：先订阅、再对账）。
   */
  function onReconnected(handler: () => void): () => void {
    reconnectedHandlers.add(handler);
    return () => reconnectedHandlers.delete(handler);
  }

  /** 连接意外断开（非 disconnect() 主动关闭）时触发：订阅方趁此对一次账、靠轮询兜底。 */
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

  function dispatch(frame: RealtimeFrame) {
    const type = typeof frame.type === "string" ? frame.type : "";
    const set = handlers.get(type);
    if (!set) return;
    for (const handler of [...set]) {
      try {
        handler(frame);
      } catch {
        // 同上：事件只是对账信号，单个处理函数抛错不该打断分发。
      }
    }
  }

  function connect() {
    intentionalClose = false;
    ensureSocket();
  }

  function disconnect() {
    intentionalClose = true;
    clearTimeout(reconnectTimer);
    const closing = socket;
    // 先摘掉引用：旧 socket 的 onclose 晚到时据此认出「不是当前连接」，不会去重连、也不会清掉新连接。
    socket = undefined;
    resetConnection();
    closing?.close(1000, "realtime closed");
  }

  function resetConnection() {
    socketOpen.value = false;
    authed.value = false;
    awaitingAck.value = 0;
    accepted.value.clear();
    resyncPending = false;
  }

  function ensureSocket() {
    // 只在浏览器里连（SSR 没有 window / WebSocket）。
    if (typeof window === "undefined" || typeof WebSocket === "undefined" || !auth.accessToken) return;
    if (socket && (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CONNECTING)) return;
    // 这条连接用的是哪张 access：被拒时据此续期（已被别的请求续过就直接用新的）。
    const sentToken = auth.accessToken;
    let rejected = false;
    const ws = new WebSocket(realtimeUrl(String(config.public.wsUrl ?? ""), String(config.public.wsApiPort ?? ""), window.location));
    socket = ws;
    resetConnection();
    socketOpen.value = true;

    ws.onopen = () => {
      if (socket !== ws) return;
      ws.send(JSON.stringify({ type: "auth", token: sentToken }));
    };
    ws.onmessage = event => {
      if (socket !== ws) return;
      let payload: RealtimeFrame;
      try {
        const parsed: unknown = JSON.parse(String(event.data));
        if (!parsed || typeof parsed !== "object") return;
        payload = parsed as RealtimeFrame;
      } catch {
        return;
      }
      const type = typeof payload.type === "string" ? payload.type : "";
      if (type === "auth.error") {
        rejected = true;
        return;
      }
      if (type === "auth.ok") {
        authed.value = true;
        resyncPending = true;
        // 重连之后一帧重发全部仍被引用的 topic；一个都没有就不发（订了也是空的），等有人 subscribe 再发。
        const topics = activeTopics();
        if (topics.length > 0) {
          awaitingAck.value += 1;
          send(subscribeFrame(topics));
        }
        return;
      }
      if (type === "subscribe.ok") {
        awaitingAck.value = Math.max(0, awaitingAck.value - 1);
        // ⚠️ 只认 topics、不认 rejected；途中已被释放的 topic 不再算订上。
        for (const topic of acceptedTopics(payload)) {
          if (refCount.has(topic)) accepted.value.add(topic);
        }
        if (accepted.value.size > 0) failures = 0;
        if (resyncPending) {
          resyncPending = false;
          emit(reconnectedHandlers);
        }
        return;
      }
      if (type === "subscription.revoked") {
        // 被移出工作区等：之后不会再有推送，订阅方据 isSubscribed 退回轮询。
        for (const topic of revokedTopics(payload)) accepted.value.delete(topic);
        return;
      }
      if (INTERNAL.has(type)) return;
      dispatch(payload);
    };
    ws.onclose = () => {
      if (socket !== ws) return;
      socket = undefined;
      resetConnection();
      if (intentionalClose) return;
      // ⚠️ 被拒（access 过期）时先续期再重连：拿着同一张过期 token 每 1.5 秒重连一次永远连不上，
      //    而界面只显示「离线」。续期被明确拒绝就不再重连 —— 会话真的失效了，下一个普通请求会送回登录页；
      //    断网续不上则照常重连（网络回来后再续）。
      const ready = rejected
        ? auth.refreshAccessToken(sentToken).then(() => true, (e: unknown) => !isDefinitiveRefreshFailure(e))
        : Promise.resolve(true);
      failures += 1;
      void ready.then(ok => {
        if (ok && !intentionalClose) {
          clearTimeout(reconnectTimer);
          reconnectTimer = setTimeout(ensureSocket, reconnectDelay(failures));
        }
      });
      emit(disconnectedHandlers);
    };
    ws.onerror = () => ws.close();
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
