import { describe, expect, it } from "vitest";
import {
  defaultKeyForm,
  expiryText,
  generateNotice,
  isScopeLocked,
  keyFormToRequest,
  keyPrefixText,
  lastUsedText,
  mcpLoadError,
  revokeConfirmText,
  roleScopeNotice,
  toggleScope,
  toggleToolset,
  toolsetOptions,
  validateKeyForm,
  type McpToolsetInfo,
} from "./mcpKeys";

const INFOS: McpToolsetInfo[] = [
  { name: "context", defaultOn: true, available: true },
  { name: "projects", defaultOn: true, available: true },
  { name: "canvas", defaultOn: true, available: true },
  { name: "catalog", defaultOn: true, available: true },
  { name: "generation", defaultOn: true, available: true },
  { name: "script", defaultOn: false, available: false },
  { name: "workflow", defaultOn: false, available: false },
  { name: "timeline", defaultOn: false, available: false },
  { name: "agent", defaultOn: false, available: false },
];

describe("scope 选择", () => {
  it("view权限必选且不可取消", () => {
    expect(isScopeLocked("seeaitv:view")).toBe(true);
    expect(toggleScope(["seeaitv:view"], "seeaitv:view", false)).toEqual(["seeaitv:view"]);
    // 就算传进来的集合里没有 view，也会补上
    expect(toggleScope([], "seeaitv:generate", true)).toEqual(["seeaitv:view", "seeaitv:generate"]);
    expect(toggleScope(["seeaitv:view", "seeaitv:edit"], "seeaitv:edit", false)).toEqual(["seeaitv:view"]);
  });

  it("结果按固定顺序", () => {
    expect(toggleScope(["seeaitv:export", "seeaitv:view"], "seeaitv:edit", true))
      .toEqual(["seeaitv:view", "seeaitv:edit", "seeaitv:export"]);
  });
});

describe("工具集", () => {
  it("默认工具集为五个默认开启集", () => {
    const form = defaultKeyForm("1", INFOS);
    expect(form.toolsets).toEqual(["context", "projects", "canvas", "catalog", "generation"]);
    expect(form.scopes).toEqual(["seeaitv:view"]);
    expect(form.expiry).toBe("90");
    // 服务端没给可用性时同样是这五个
    expect(defaultKeyForm("1").toolsets).toEqual(["context", "projects", "canvas", "catalog", "generation"]);
  });

  it("没有工具的工具集标后续开放且勾不上", () => {
    const options = toolsetOptions(INFOS);
    expect(options.find((o) => o.name === "script")).toMatchObject({ selectable: false, hint: "后续开放" });
    expect(options.find((o) => o.name === "canvas")).toMatchObject({ selectable: true, hint: "" });
    expect(toggleToolset(["canvas"], "script", true, INFOS)).toEqual(["canvas"]);
    expect(toggleToolset(["canvas", "context"], "canvas", false, INFOS)).toEqual(["context"]);
  });

  it("可用性按服务端给的而不是写死", () => {
    const infos = INFOS.map((i) => (i.name === "script" ? { ...i, available: true } : i));
    expect(toggleToolset(["canvas"], "script", true, infos)).toEqual(["canvas", "script"]);
  });
});

describe("表单 → 请求体", () => {
  it("表单转请求体不含明文字段", () => {
    const form = { ...defaultKeyForm("123", INFOS), clientName: "  我的 Claude Code  " };
    const body = keyFormToRequest(form);
    expect(body).toEqual({
      workspaceId: "123",
      clientName: "我的 Claude Code",
      scopes: ["seeaitv:view"],
      toolsets: ["context", "projects", "canvas", "catalog", "generation"],
      requirePageConfirm: false,
      neverExpires: false,
      expiresInDays: 90,
    });
    expect(Object.keys(body)).not.toContain("plaintext");
    expect(JSON.stringify(body)).not.toContain("sk_mcp_");
  });

  it("永久不带天数，30 天带天数", () => {
    const base = { ...defaultKeyForm("1", INFOS), clientName: "x" };
    const never = keyFormToRequest({ ...base, expiry: "never" });
    expect(never.neverExpires).toBe(true);
    expect("expiresInDays" in never).toBe(false);
    expect(keyFormToRequest({ ...base, expiry: "30" }).expiresInDays).toBe(30);
  });

  it("页面确认开关默认关闭，勾上就随请求发出", () => {
    expect(defaultKeyForm("1").requirePageConfirm).toBe(false);
    expect(keyFormToRequest({ ...defaultKeyForm("1"), clientName: "x" }).requirePageConfirm).toBe(false);
    expect(keyFormToRequest({ ...defaultKeyForm("1"), clientName: "x", requirePageConfirm: true }).requirePageConfirm).toBe(true);
  });

  it("校验名称与工作区", () => {
    expect(validateKeyForm({ ...defaultKeyForm(""), clientName: "x" })).toContain("工作区");
    expect(validateKeyForm({ ...defaultKeyForm("1"), clientName: "  " })).toContain("名称");
    expect(validateKeyForm({ ...defaultKeyForm("1"), clientName: "a".repeat(101) })).toContain("100");
    expect(validateKeyForm({ ...defaultKeyForm("1"), clientName: "ok", toolsets: [] })).toContain("工具集");
    expect(validateKeyForm({ ...defaultKeyForm("1"), clientName: "ok" })).toBeNull();
  });
});

describe("列表文案", () => {
  it("到期文案永久与已过期", () => {
    expect(expiryText({ status: "active", expiresAt: null })).toBe("永久");
    expect(expiryText({ status: "expired", expiresAt: "2026-01-01T00:00:00Z" })).toBe("已过期");
    expect(expiryText({ status: "revoked", expiresAt: null })).toBe("已撤销");
    // ⚠️ 只看服务端 status，不拿本机时钟比：即使日期在过去，服务端说 active 就显示日期
    expect(expiryText({ status: "active", expiresAt: "2000-06-15T12:00:00Z" })).toMatch(/^2000-06-1[56] 到期$/);
  });

  it("从未使用文案", () => {
    expect(lastUsedText(null)).toBe("从未使用");
    expect(lastUsedText(undefined)).toBe("从未使用");
    expect(lastUsedText("2026-10-17T08:00:00Z")).toMatch(/^2026-10-1[78] \d{2}:\d{2}$/);
  });

  it("前缀显示与撤销后果", () => {
    expect(keyPrefixText("ab12cd34")).toBe("sk_mcp_ab12cd34…");
    const text = revokeConfirmText({ clientName: "我的 Cursor", prefix: "ab12cd34" });
    expect(text).toContain("下一次调用即失效");
    expect(text).toContain("我的 Cursor");
  });
});

describe("角色与花钱提示", () => {
  it("只读角色选generate给出不生效提示", () => {
    const notice = roleScopeNotice("viewer", ["seeaitv:view", "seeaitv:edit", "seeaitv:generate"]);
    expect(notice).toContain("你在该工作区是只读");
    expect(notice).toContain("编辑、生成权限不会生效");
  });

  it("编辑者与只勾查看时没有提示", () => {
    expect(roleScopeNotice("editor", ["seeaitv:view", "seeaitv:generate", "seeaitv:export"])).toBeNull();
    expect(roleScopeNotice("viewer", ["seeaitv:view"])).toBeNull();
  });

  it("选了generate醒目提示会花积分", () => {
    expect(generateNotice(["seeaitv:view", "seeaitv:generate"])).toContain("积分");
    expect(generateNotice(["seeaitv:view"])).toBeNull();
  });
});

describe("加载失败", () => {
  it("断网说断网、403 与 404 不给重试", () => {
    expect(mcpLoadError(new Error("x"), false)).toEqual({ text: "网络已断开，恢复后点重试", retry: true });
    expect(mcpLoadError({ statusCode: 403, message: "无权" }, true).retry).toBe(false);
    expect(mcpLoadError({ statusCode: 404 }, true).text).toContain("暂未开放");
    expect(mcpLoadError({ statusCode: 500, message: "boom", data: { code: 50001 } }, true))
      .toEqual({ text: "加载失败：boom（50001）", retry: true });
  });
});
