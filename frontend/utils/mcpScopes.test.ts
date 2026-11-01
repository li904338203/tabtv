import { describe, expect, it } from "vitest";
import {
  clientSource,
  consentAllowBody,
  consentDenyBody,
  consentError,
  consentRoleNotice,
  consentScopeOptions,
  consentToolsetOptions,
  defaultConsentToolsets,
  initialConsentForm,
  scopeRoleHint,
  spendNotice,
  toggleConsentScope,
  toggleConsentToolset,
  validateConsentForm,
} from "./mcpScopes";
import type { McpToolsetInfo } from "./mcpKeys";

const ALL = ["seeaitv:view", "seeaitv:edit", "seeaitv:generate", "seeaitv:export"] as const;

describe("mcpScopes", () => {
  it("viewer 勾 generate 标为不生效", () => {
    expect(scopeRoleHint("viewer", "seeaitv:generate")).toBe("你在该工作区是只读，勾选也不会生效");
    expect(scopeRoleHint("viewer", "seeaitv:view")).toBeNull();
    expect(scopeRoleHint("editor", "seeaitv:generate")).toBeNull();
    const opts = consentScopeOptions([...ALL], "viewer");
    expect(opts.find((o) => o.value === "seeaitv:generate")?.roleHint).toContain("只读");
    expect(opts.find((o) => o.value === "seeaitv:view")?.locked).toBe(true);
    expect(consentRoleNotice("viewer", ["seeaitv:view", "seeaitv:generate"])).toContain("生成权限勾选也不会生效");
    expect(consentRoleNotice("owner", [...ALL])).toBeNull();
  });

  it("只列客户端申请了的 scope，view 总在", () => {
    expect(consentScopeOptions(["seeaitv:generate"], "owner").map((o) => o.value)).toEqual(["seeaitv:view", "seeaitv:generate"]);
  });

  it("勾 generate 出现团队积分说明", () => {
    expect(spendNotice(["seeaitv:view"], { name: "制作组", kind: "team" })).toBeNull();
    const team = spendNotice(["seeaitv:view", "seeaitv:generate"], { name: "制作组", kind: "team" });
    expect(team?.text).toContain("消耗「制作组」的积分（付费账户：团队）");
    expect(team?.text).toContain("每一次都会先请你确认金额");
    expect(team?.team).toBe("消耗的是团队积分，团队管理员可在用量审计里看到是你");
    const personal = spendNotice(["seeaitv:generate"], { name: "个人", kind: "personal" });
    expect(personal?.text).toContain("付费账户：个人");
    expect(personal?.team).toBeNull();
  });

  it("默认工具集", () => {
    expect(defaultConsentToolsets({ toolset: "" })).toEqual(["context", "projects", "canvas", "catalog", "generation"]);
    // 客户端连的是 /mcp/x/script：把它也勾上。
    expect(defaultConsentToolsets({ toolset: "script" })).toContain("script");
    // 服务端说不可用的不勾、不可选。
    const infos: McpToolsetInfo[] = [
      { name: "context", defaultOn: true, available: true },
      { name: "generation", defaultOn: true, available: false },
    ];
    expect(defaultConsentToolsets({ toolset: "" }, infos)).toEqual(["context"]);
    expect(consentToolsetOptions(infos).find((o) => o.name === "generation")).toMatchObject({ selectable: false, hint: "后续开放" });
    expect(toggleConsentToolset(["context"], "generation", true, infos)).toEqual(["context"]);
    expect(toggleConsentToolset(["context"], "script", true)).toEqual(["context", "script"]);
  });

  it("初始表单：当前工作区、申请的全部 scope、页面确认默认关", () => {
    const form = initialConsentForm({ requestedScopes: ["seeaitv:generate"], toolset: "" }, [{ id: "1" }, { id: "2" }], "2");
    expect(form).toMatchObject({ workspaceId: "2", scopes: ["seeaitv:view", "seeaitv:generate"], requirePageConfirm: false });
    expect(initialConsentForm({ requestedScopes: [], toolset: "" }, [{ id: "1" }], "9").workspaceId).toBe("1");
  });

  it("view 取消不掉", () => {
    expect(toggleConsentScope(["seeaitv:view", "seeaitv:edit"], "seeaitv:view", false)).toEqual(["seeaitv:view", "seeaitv:edit"]);
  });

  it("请求体：允许带勾选，拒绝只带句柄", () => {
    const form = { workspaceId: "7", scopes: ["seeaitv:generate" as const], toolsets: ["context" as const], requirePageConfirm: true };
    expect(consentAllowBody("h", form)).toEqual({
      req: "h", decision: "allow", workspaceId: "7", scopes: ["seeaitv:view", "seeaitv:generate"],
      toolsets: ["context"], requirePageConfirm: true,
    });
    expect(consentDenyBody("h")).toEqual({ req: "h", decision: "deny" });
    expect(validateConsentForm({ ...form, workspaceId: "" })).toBe("请选择要授权的工作区");
    expect(validateConsentForm({ ...form, toolsets: [] })).toBe("至少选择一个工具集");
    expect(validateConsentForm(form)).toBeNull();
  });

  it("DCR 客户端是未验证来源", () => {
    expect(clientSource({ clientId: "https://a.dev/c.json", name: "A", registration: "cimd", verified: true, host: "a.dev", clientUri: "" }))
      .toEqual({ verified: true, text: "已验证来源：a.dev" });
    const dcr = clientSource({ clientId: "x", name: "Claude", registration: "dcr", verified: false, host: "", clientUri: "" });
    expect(dcr.verified).toBe(false);
    expect(dcr.text).toContain("未验证的客户端");
  });

  it("句柄过期不给重试", () => {
    expect(consentError({ data: { code: 40007 } }, true)).toEqual({ text: "授权请求已过期，请回到客户端重新连接", retry: false, expired: true });
    expect(consentError({}, false).text).toContain("网络已断开");
    expect(consentError({ statusCode: 500, message: "boom", data: { code: 50001 } }, true)).toMatchObject({ retry: true, text: "出错了：boom（50001）" });
  });
});
