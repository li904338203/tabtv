import { describe, expect, it } from "vitest";
import {
  CANVAS_TOGGLE_KEY,
  agentCanvasViewPath,
  agentChatPath,
  agentLandingPath,
  documentDisplayName,
  documentSizeText,
  parseNodeParam,
  parseSessionParam,
  readCanvasToggle,
  sortDocuments,
  writeCanvasToggle
} from "./agentChat";
import { agentCanvasPath } from "./quickGenerate";

function memoryStorage(initial: Record<string, string> = {}) {
  const data = new Map(Object.entries(initial));
  return {
    getItem: (key: string) => data.get(key) ?? null,
    setItem: (key: string, value: string) => { data.set(key, value); },
    data
  };
}

describe("路由拼接", () => {
  it("对话页带会话参数，没有会话就不带", () => {
    expect(agentChatPath("123", "456")).toBe("/video/chat/123?session=456");
    expect(agentChatPath("123")).toBe("/video/chat/123");
    expect(agentChatPath("123", "")).toBe("/video/chat/123");
  });

  it("画布页带会话与节点；与创造页原有的跳转地址一致", () => {
    expect(agentCanvasViewPath("1", { sessionId: "2" })).toBe(agentCanvasPath("1", "2"));
    expect(agentCanvasViewPath("1", { sessionId: "2", nodeIds: ["n_a", "n-b"] })).toBe("/video/canvas/1?session=2&node=n_a,n-b");
    expect(agentCanvasViewPath("1", { nodeIds: ["x"] })).toBe("/video/canvas/1?node=x");
    expect(agentCanvasViewPath("1")).toBe("/video/canvas/1");
  });

  it("不合法的节点 ID 不进地址", () => {
    expect(agentCanvasViewPath("1", { nodeIds: ["a b", "<x>", "ok"] })).toBe("/video/canvas/1?node=ok");
  });

  it("开关决定发送后去哪", () => {
    expect(agentLandingPath("7", "8", true)).toBe("/video/canvas/7?session=8");
    expect(agentLandingPath("7", "8", false)).toBe("/video/chat/7?session=8");
  });
});

describe("查询参数解析", () => {
  it("会话只收十进制", () => {
    expect(parseSessionParam("123")).toBe("123");
    expect(parseSessionParam(["9", "8"])).toBe("9");
    expect(parseSessionParam("12a")).toBeUndefined();
    expect(parseSessionParam(undefined)).toBeUndefined();
    expect(parseSessionParam("")).toBeUndefined();
  });

  it("节点逗号分隔、去重、丢掉不合法的", () => {
    expect(parseNodeParam("a,b,a, c ")).toEqual(["a", "b", "c"]);
    expect(parseNodeParam("ok,../x,<script>")).toEqual(["ok"]);
    expect(parseNodeParam(undefined)).toEqual([]);
    expect(parseNodeParam(["n1"])).toEqual(["n1"]);
  });

  it("节点最多 20 个", () => {
    expect(parseNodeParam(Array.from({ length: 30 }, (_, i) => `n${i}`).join(","))).toHaveLength(20);
  });
});

describe("「画布」开关记忆", () => {
  it("默认开", () => {
    expect(readCanvasToggle(memoryStorage())).toBe(true);
    expect(readCanvasToggle(undefined)).toBe(true);
  });

  it("关掉后读回来仍是关，再打开是开", () => {
    const storage = memoryStorage();
    writeCanvasToggle(storage, false);
    expect(storage.data.get(CANVAS_TOGGLE_KEY)).toBe("0");
    expect(readCanvasToggle(storage)).toBe(false);
    writeCanvasToggle(storage, true);
    expect(readCanvasToggle(storage)).toBe(true);
  });

  it("存坏了按默认开；读写抛错不影响使用", () => {
    expect(readCanvasToggle(memoryStorage({ [CANVAS_TOGGLE_KEY]: "garbage" }))).toBe(true);
    const broken = { getItem: () => { throw new Error("denied"); }, setItem: () => { throw new Error("denied"); } };
    expect(readCanvasToggle(broken)).toBe(true);
    expect(() => writeCanvasToggle(broken, false)).not.toThrow();
  });
});

describe("对话文件", () => {
  it("plan.md 置顶、其余按更新时间倒序、项目记忆最后", () => {
    const sorted = sortDocuments([
      { path: "/.memory.md", updatedAt: "2026-10-16T10:00:00Z" },
      { path: "/notes/a.md", updatedAt: "2026-10-15T10:00:00Z" },
      { path: "/shots.md", updatedAt: "2026-10-16T09:00:00Z" },
      { path: "/plan.md", updatedAt: "2026-10-01T00:00:00Z" },
      { path: "/notes/bad.md", updatedAt: "not a date" }
    ]);
    expect(sorted.map(item => item.path)).toEqual(["/plan.md", "/shots.md", "/notes/a.md", "/notes/bad.md", "/.memory.md"]);
  });

  it("文件名与大小", () => {
    expect(documentDisplayName("/plan.md")).toBe("plan.md");
    expect(documentDisplayName("/notes/x.md")).toBe("notes/x.md");
    expect(documentDisplayName("/.memory.md")).toBe("项目记忆");
    expect(documentSizeText(512)).toBe("512 B");
    expect(documentSizeText(2048)).toBe("2.0 KB");
    expect(documentSizeText(20 * 1024)).toBe("20 KB");
    expect(documentSizeText(-1)).toBe("");
  });
});
