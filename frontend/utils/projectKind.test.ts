import { describe, expect, it } from "vitest";
import type { components } from "@tabtv/api-client";
import {
  PROJECT_KIND_KEY,
  agentSessionTitle,
  agentStateText,
  agentStateTone,
  isMarketingProject,
  normalizeProjectKind,
  PROJECT_KIND_OPTIONS,
  projectBadgeVariant,
  projectCanvasPath,
  projectKindEmptyText,
  projectOpenPath,
  readProjectKind,
  writeProjectKind
} from "./projectKind";

function memoryStorage(initial: Record<string, string> = {}) {
  const data = { ...initial };
  return {
    data,
    getItem: (key: string) => (key in data ? data[key]! : null),
    setItem: (key: string, value: string) => { data[key] = value; }
  };
}

describe("类型筛选", () => {
  it("选项顺序：全部 ｜ 画布 ｜ Agent ｜ 营销", () => {
    expect(PROJECT_KIND_OPTIONS.map(item => item.name)).toEqual(["全部", "画布", "Agent", "营销"]);
    expect(PROJECT_KIND_OPTIONS.map(item => item.code)).toEqual(["all", "canvas", "agent", "marketing"]);
  });

  it("只认 canvas / agent / marketing，其余一律全部", () => {
    expect(normalizeProjectKind("marketing")).toBe("marketing");
    expect(normalizeProjectKind("Marketing")).toBe("all");
    expect(normalizeProjectKind("agent")).toBe("agent");
    expect(normalizeProjectKind("canvas")).toBe("canvas");
    expect(normalizeProjectKind("all")).toBe("all");
    expect(normalizeProjectKind("Agent")).toBe("all");
    expect(normalizeProjectKind(null)).toBe("all");
  });

  it("记住上次的选择；存坏了或读抛错都按全部", () => {
    const storage = memoryStorage();
    expect(readProjectKind(storage)).toBe("all");
    writeProjectKind(storage, "agent");
    expect(storage.data[PROJECT_KIND_KEY]).toBe("agent");
    expect(readProjectKind(storage)).toBe("agent");
    expect(readProjectKind(memoryStorage({ [PROJECT_KIND_KEY]: "garbage" }))).toBe("all");
    expect(readProjectKind({ getItem: () => { throw new Error("denied"); } })).toBe("all");
    expect(readProjectKind(undefined)).toBe("all");
    expect(() => writeProjectKind({ getItem: () => null, setItem: () => { throw new Error("quota"); } }, "canvas")).not.toThrow();
  });

  it("空列表的话说清是哪一类", () => {
    expect(projectKindEmptyText("agent")).toContain("Agent");
    expect(projectKindEmptyText("canvas")).toContain("画布");
    expect(projectKindEmptyText("marketing")).toBe("还没有营销项目");
    expect(projectKindEmptyText("all")).toBe("这里还没有项目");
  });
});

describe("营销角标", () => {
  it("只看服务端 origin：营销项目只显示「营销」一个角标", () => {
    expect(isMarketingProject({ origin: "marketing" })).toBe(true);
    expect(projectBadgeVariant({ origin: "marketing", agent: { sessionId: "1" } })).toBe("marketing");
    // 理论上营销项目必有会话；即便没有也按落列的来源显示
    expect(projectBadgeVariant({ origin: "marketing", agent: null })).toBe("marketing");
  });

  it("不按名字 / 会话推断：origin 为空时营销会话也只是 Agent 项目", () => {
    expect(isMarketingProject({ origin: null })).toBe(false);
    expect(isMarketingProject({})).toBe(false);
    expect(isMarketingProject({ origin: "Marketing" })).toBe(false);
    expect(projectBadgeVariant({ origin: null, agent: { sessionId: "1" } })).toBe("agent");
    expect(projectBadgeVariant({ origin: null, agent: null })).toBeNull();
    expect(projectBadgeVariant({})).toBeNull();
  });
  // 首页最近项目（/home 的 recent[]）与全部项目页同一个判定（MKT-T-32）：契约里的 origin 直接喂进来。
  it("首页最近项目按契约 RecentProject.origin 显示「营销」", () => {
    const base: components["schemas"]["RecentProject"] = {
      id: "1", name: "x", updatedAt: "2026-10-21T00:00:00Z", agent: null, origin: null
    };
    expect(projectBadgeVariant({ ...base, origin: "marketing" })).toBe("marketing");
    expect(projectBadgeVariant(base)).toBeNull();
    expect(projectBadgeVariant({ ...base, agent: { sessionId: "9", title: "", state: "idle", lastActiveAt: "2026-10-21T00:00:00Z" } })).toBe("agent");
  });
});

describe("会话状态文案", () => {
  it.each([
    ["planning", "进行中", "running"],
    ["executing", "进行中", "running"],
    ["waiting_task", "进行中", "running"],
    ["awaiting_input", "等你确认", "waiting"],
    ["awaiting_approval", "等你确认", "waiting"],
    ["idle", "空闲", "idle"],
    ["failed", "失败", "failed"],
    ["cancelled", "已结束", "ended"]
  ])("%s → %s", (state, text, tone) => {
    expect(agentStateText(state)).toBe(text);
    expect(agentStateTone(state)).toBe(tone);
  });

  it("未知 / 缺省状态按空闲，不显示空白", () => {
    expect(agentStateText("some_new_state")).toBe("空闲");
    expect(agentStateText(undefined)).toBe("空闲");
    expect(agentStateText("")).toBe("空闲");
  });

  it("会话标题为空时有兜底", () => {
    expect(agentSessionTitle({ title: "  " })).toBe("未命名会话");
    expect(agentSessionTitle(null)).toBe("未命名会话");
    expect(agentSessionTitle({ title: "做一条短片" })).toBe("做一条短片");
  });
});

describe("点卡片去哪", () => {
  it("Agent 项目进对话页的最近会话，普通项目进画布", () => {
    expect(projectOpenPath({ id: "11", agent: { sessionId: "22" } })).toBe("/video/chat/11?session=22");
    expect(projectOpenPath({ id: "11", agent: null })).toBe("/video/canvas/11");
    expect(projectOpenPath({ id: "11" })).toBe("/video/canvas/11");
  });

  it("「打开画布」带上最近会话（面板展开在同一个会话）", () => {
    expect(projectCanvasPath({ id: "11", agent: { sessionId: "22" } })).toBe("/video/canvas/11?session=22");
    expect(projectCanvasPath({ id: "11", agent: null })).toBe("/video/canvas/11");
  });
});
