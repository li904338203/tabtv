import { describe, expect, it } from "vitest";

import {
  acceptedTopics,
  eventTarget,
  projectTopic,
  realtimeUrl,
  reconnectDelay,
  revoked,
  revokedTopics,
  subscribeFrame,
  subscribed,
  unsubscribeFrame,
  workspaceTopic,
} from "./realtime";

describe("实时通道的地址", () => {
  const page = (href: string) => {
    const u = new URL(href);
    return { protocol: u.protocol, hostname: u.hostname, host: u.host };
  };

  it("开发时跟着页面的主机名走、补上 API 端口：从局域网地址打开也连得上（此前写死 localhost）", () => {
    expect(realtimeUrl("", "8180", page("http://192.168.101.95:3000/video/canvas/1"))).toBe("ws://192.168.101.95:8180/ws/events");
    expect(realtimeUrl("", "8180", page("http://localhost:3000/"))).toBe("ws://localhost:8180/ws/events");
    expect(realtimeUrl("", "8180", page("http://[::1]:3000/"))).toBe("ws://[::1]:8180/ws/events");
  });

  it("生产不补端口（经反向代理同域），https 页面用 wss", () => {
    expect(realtimeUrl("", "", page("https://app.example.com/video"))).toBe("wss://app.example.com/ws/events");
  });

  it("显式配了完整地址就照用", () => {
    expect(realtimeUrl("wss://rt.example.com/ws/events", "8180", page("http://localhost:3000/"))).toBe("wss://rt.example.com/ws/events");
  });
});

describe("实时通道的订阅", () => {
  it("按新协议订工作区 topic（老前端发的 projectId 新后端不认 —— 订了个寂寞）", () => {
    const frame = subscribeFrame(workspaceTopic("96209082733559810"));
    expect(frame).toEqual({ type: "subscribe", topics: ["workspace:96209082733559810"] });
    expect(frame).not.toHaveProperty("projectId");
  });

  it("subscribe.ok 只有真的订上了才算在线：被拒的也回 subscribe.ok", () => {
    const topic = workspaceTopic("9");
    expect(subscribed({ type: "subscribe.ok", topics: [topic], rejected: [] }, topic)).toBe(true);
    expect(subscribed({ type: "subscribe.ok", topics: [], rejected: [{ topic, code: 40301 }] }, topic)).toBe(false);
    // 老协议下服务端回的就是这个：什么都没订上，界面却显示「在线」
    expect(subscribed({ type: "subscribe.ok", topics: [], rejected: [] }, topic)).toBe(false);
    expect(subscribed({ type: "auth.ok" }, topic)).toBe(false);
  });

  it("连不上时重连退避：第一次照旧 1.5 秒，之后翻倍，封顶 30 秒（不再每分钟 40 次握手失败）", () => {
    expect([1, 2, 3, 4, 5, 6, 20].map(reconnectDelay)).toEqual([1500, 3000, 6000, 12000, 24000, 30000, 30000]);
  });

  it("subscription.revoked 撤销了本页的 topic 才算", () => {
    const topic = workspaceTopic("9");
    expect(revoked({ type: "subscription.revoked", topics: [topic] }, topic)).toBe(true);
    expect(revoked({ type: "subscription.revoked", topics: ["workspace:8"] }, topic)).toBe(false);
  });
});

describe("多 topic 订阅", () => {
  it("多 topic 订阅帧：重连后一帧重发全部仍被引用的 topic", () => {
    expect(subscribeFrame([workspaceTopic("9"), projectTopic("5")])).toEqual({ type: "subscribe", topics: ["workspace:9", "project:5"] });
    // 旧签名（单 topic）照旧可用
    expect(subscribeFrame("workspace:9")).toEqual({ type: "subscribe", topics: ["workspace:9"] });
  });

  it("退订帧", () => {
    expect(unsubscribeFrame(["workspace:9"])).toEqual({ type: "unsubscribe", topics: ["workspace:9"] });
    expect(unsubscribeFrame("project:5")).toEqual({ type: "unsubscribe", topics: ["project:5"] });
  });

  it("acceptedTopics 只取 topics 不取 rejected", () => {
    const frame = { type: "subscribe.ok", topics: ["workspace:9"], rejected: [{ topic: "project:5", code: 40301 }] };
    expect(acceptedTopics(frame)).toEqual(["workspace:9"]);
    expect(acceptedTopics({ type: "subscribe.ok", topics: [], rejected: [{ topic: "workspace:9", code: 40301 }] })).toEqual([]);
    // 不是 subscribe.ok、或 topics 不是数组：一个都不算
    expect(acceptedTopics({ type: "subscription.revoked", topics: ["workspace:9"] })).toEqual([]);
    expect(acceptedTopics({ type: "subscribe.ok" })).toEqual([]);
    expect(acceptedTopics(null)).toEqual([]);
  });

  it("revokedTopics 只认 subscription.revoked", () => {
    expect(revokedTopics({ type: "subscription.revoked", topics: ["workspace:9"], reason: "member_removed" })).toEqual(["workspace:9"]);
    expect(revokedTopics({ type: "subscribe.ok", topics: ["workspace:9"] })).toEqual([]);
  });
});

describe("生成事件的处理", () => {
  const tracked = new Map([["701", "node-a"]]);
  const update = (over: Record<string, unknown> = {}) => ({
    type: "generation.update", generationId: "701", projectId: "p1", nodeId: "node-from-event", status: "success", ...over,
  });

  it("本页在跟踪的任务：落到本页记下的那个节点（不信事件里的 nodeId）", () => {
    expect(eventTarget(update(), "p1", tracked)).toEqual({ generationId: "701", nodeId: "node-a" });
  });

  it("已经落定、不再跟踪的任务：晚到的旧事件丢掉，不把完成的节点打回运行中", () => {
    expect(eventTarget(update({ generationId: "700", status: "running" }), "p1", tracked)).toBeNull();
  });

  it("同一工作区别的项目的事件：丢掉", () => {
    expect(eventTarget(update({ projectId: "p2" }), "p1", tracked)).toBeNull();
  });

  it("不是生成事件、或缺 generationId：丢掉", () => {
    expect(eventTarget({ type: "workflow.update", generationId: "701", projectId: "p1" }, "p1", tracked)).toBeNull();
    expect(eventTarget({ type: "generation.update", projectId: "p1" }, "p1", tracked)).toBeNull();
    expect(eventTarget(null, "p1", tracked)).toBeNull();
  });
});
