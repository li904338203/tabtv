import { describe, expect, it } from "vitest";
import {
  APPROVED_TEXT,
  EXPIRED_TEXT,
  NOT_OWNER_TEXT,
  REJECTED_TEXT,
  USED_TEXT,
  afterText,
  approveErrorText,
  errorScreen,
  payerText,
  stateScreen,
  type McpApproval,
} from "./mcpApproval";

function approval(over: Partial<McpApproval> = {}): McpApproval {
  return {
    quoteId: "9", state: "pending", clientName: "Claude Code", workspaceId: "2", workspaceName: "制作组",
    workspaceKind: "team", tool: "generate", summary: "用模型 img-m 执行 text2img", items: [{ label: "img-m · text2img", credits: 14 }],
    credits: 14, payerAccountId: "3", balance: 100, after: 86, shortfall: 0, expiresAt: "2026-10-17T08:10:00Z",
    approvedAt: null, rejectedAt: null, usedAt: null, ...over,
  };
}

describe("mcpApproval", () => {
  it("非本人不显示金额", () => {
    const s = errorScreen({ statusCode: 403, data: { code: 40309 } }, true);
    expect(s.kind).toBe("not_owner");
    expect(s.text).toBe(NOT_OWNER_TEXT);
    expect("approval" in s).toBe(false);
    expect(JSON.stringify(s)).not.toMatch(/Claude|积分|\d{2,}/);
  });

  it("已过期文案", () => {
    const s = stateScreen(approval({ state: "expired" }));
    expect(s.text).toBe(EXPIRED_TEXT);
    expect(s.text).toContain("有效期 10 分钟");
    expect(s.canApprove || s.canReject).toBe(false);
  });

  it("已使用文案", () => {
    const s = stateScreen(approval({ state: "used" }));
    expect(s.text).toBe(USED_TEXT);
    expect(s.canApprove || s.canReject).toBe(false);
    expect(stateScreen(approval({ state: "rejected" })).text).toBe(REJECTED_TEXT);
  });

  it("余额不足禁用批准并给差额", () => {
    const s = stateScreen(approval({ balance: 5, after: -9, shortfall: 9 }));
    expect(s.canApprove).toBe(false);
    expect(s.canReject).toBe(true);
    expect(s.approveBlocked).toBe("余额不足，还差 9 积分（本次需要 14，当前余额 5）。补足积分后再批准");
    expect(afterText({ after: -9, shortfall: 9 })).toBe("不足，还差 9");
    expect(approveErrorText({ data: { code: 42201, details: { shortfall: 9 } } })).toBe("余额不足，还差 9 积分，补足后再批准");
    expect(approveErrorText({ data: { code: 40001 } })).toBeNull();
  });

  it("已批准未使用提示回客户端", () => {
    const s = stateScreen(approval({ state: "approved", approvedAt: "2026-10-17T08:01:00Z" }));
    expect(s.text).toBe(APPROVED_TEXT);
    expect(s.tone).toBe("success");
    expect(s.canApprove).toBe(false);
  });

  it("待确认可批准、可拒绝，并写清花谁的钱", () => {
    const s = stateScreen(approval());
    expect(s.canApprove && s.canReject).toBe(true);
    expect(s.text).toContain("「Claude Code」请求在「制作组」里花费 14 积分");
    expect(payerText(approval())).toContain("团队管理员可在用量审计里看到是你");
    expect(payerText(approval({ workspaceKind: "personal", workspaceName: "个人" }))).toBe("个人空间「个人」的积分");
  });

  it("不存在 / 连接撤销 / 断网各有说法", () => {
    expect(errorScreen({ statusCode: 404, data: { code: 40401 } }, true).kind).toBe("not_found");
    expect(errorScreen({ statusCode: 409, data: { code: 40905 } }, true).kind).toBe("revoked");
    expect(errorScreen({}, false)).toMatchObject({ kind: "error", retry: true });
  });
});
