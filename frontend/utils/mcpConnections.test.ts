import { describe, expect, it } from "vitest";
import {
  canManageWorkspaceMcp,
  clientSourceText,
  connectionUserText,
  grantRevokeConfirmText,
  oauthGrants,
  pageConfirmOffText,
  workspaceSwitchNotice,
  workspaceSwitchText,
  type McpGrant,
} from "./mcpConnections";

const grant = (over: Partial<McpGrant> = {}): McpGrant => ({
  id: "1", workspaceId: "7", clientName: "Claude Desktop", source: "oauth", clientRegistration: "cimd",
  clientHost: "claude.ai", keyPrefix: null, scopes: ["seeaitv:view"], toolsets: ["context"],
  requirePageConfirm: false, status: "active", lastUsedAt: null, createdAt: "2026-10-17T00:00:00Z", revokedAt: null,
  ...over,
});

describe("工作区分区", () => {
  it("只有 owner / admin 显示本工作区连接分区", () => {
    expect(canManageWorkspaceMcp("owner")).toBe(true);
    expect(canManageWorkspaceMcp("admin")).toBe(true);
    expect(canManageWorkspaceMcp("editor")).toBe(false);
    expect(canManageWorkspaceMcp("viewer")).toBe(false);
    expect(canManageWorkspaceMcp(undefined)).toBe(false);
  });
  it("平台关闭时如实说明工作区开着也没用；工作区关闭时说明连接仍保留", () => {
    expect(workspaceSwitchNotice({ enabled: true, platformEnabled: false })).toContain("平台已暂停");
    expect(workspaceSwitchNotice({ enabled: false, platformEnabled: true })).toContain("连接仍保留");
    expect(workspaceSwitchNotice({ enabled: true, platformEnabled: true })).toBeNull();
    expect(workspaceSwitchText(false)).toContain("不会撤销任何连接");
    expect(workspaceSwitchText(false)).toContain("站内功能不受影响");
  });
});

describe("已连接的客户端", () => {
  it("只列 OAuth（访问密钥在密钥列表里已有一行）", () => {
    const list = oauthGrants([grant(), grant({ id: "2", source: "access_key", clientRegistration: null, keyPrefix: "abcd1234" })]);
    expect(list.map((g) => g.id)).toEqual(["1"]);
  });
  it("客户端来源：CIMD 给域名、DCR 说未验证", () => {
    expect(clientSourceText(grant())).toBe("已验证来源：claude.ai");
    expect(clientSourceText(grant({ clientRegistration: "dcr", clientHost: "" }))).toContain("未验证");
    expect(clientSourceText(grant({ source: "access_key", clientRegistration: null, keyPrefix: "abcd1234" }))).toContain("sk_mcp_abcd1234");
  });
  it("撤销确认含客户端名与后果，工作区管理员撤销时点名是谁的", () => {
    expect(grantRevokeConfirmText(grant())).toContain("「Claude Desktop」");
    expect(grantRevokeConfirmText(grant())).toContain("下一次调用即失效");
    expect(grantRevokeConfirmText(grant(), "同事")).toContain("同事的");
    expect(grantRevokeConfirmText(grant({ source: "access_key" }), "同事")).toContain("访问密钥");
  });
  it("已离开的成员留下 ID", () => {
    expect(connectionUserText({ userId: "9", userName: null })).toBe("已离开的成员（9）");
    expect(connectionUserText({ userId: "9", userName: "小王" })).toBe("小王");
  });
  it("关闭页面确认前的二次确认点名客户端", () => {
    expect(pageConfirmOffText(grant())).toContain("「Claude Desktop」");
  });
});
