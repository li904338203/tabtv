import { describe, expect, it } from "vitest";
import {
  GUIDE_MODES,
  HERO_CLIENTS,
  clientInitial,
  guideClients,
  guideCopyText,
  modeOf,
  pickClient,
  unsupportedText,
  type McpClientMode,
} from "./mcpGuide";
import type { McpClientSnippets } from "./mcpSnippets";

const PH = "<YOUR_MCP_KEY>";
const URL = "https://mcp.example.com/mcp";

function mode(m: "oauth" | "key", over: Partial<McpClientMode> = {}): McpClientMode {
  const key = m === "key";
  return {
    mode: m,
    supported: true,
    format: "json",
    location: "x",
    content: key ? `{"Authorization":"Bearer ${PH}"}` : `{"url":"${URL}"}`,
    agentPrompt: key ? `帮我接入 seeAitv MCP。\n1. 写入 Bearer ${PH}` : `帮我接入 seeAitv MCP。\n1. claude mcp add seeaitv '${URL}'`,
    note: "",
    ...over,
  };
}

function res(): McpClientSnippets {
  const base = { format: "json" as const, location: "x", content: "", note: "" };
  return {
    mcpUrl: URL,
    mcpUrlConfigured: true,
    keyPlaceholder: PH,
    toolsets: [],
    items: [
      { ...base, client: "stdio_bridge", label: "stdio 桥接", modes: [mode("key")] },
      { ...base, client: "workbuddy", label: "WorkBuddy", modes: [mode("oauth", { supported: false, content: "", agentPrompt: "", note: "待实测：请用密钥" }), mode("key")] },
      { ...base, client: "codex", label: "Codex", modes: [mode("oauth"), mode("key")] },
      { ...base, client: "claude_code", label: "Claude Code", modes: [mode("oauth"), mode("key")] },
    ],
  };
}

describe("页签与方式", () => {
  it("一键接入在前，密钥在后", () => {
    expect(GUIDE_MODES.map((m) => m.mode)).toEqual(["oauth", "key"]);
  });

  it("按固定顺序列客户端；stdio 桥接只在密钥方式出现", () => {
    expect(guideClients(res(), "oauth").map((c) => c.client)).toEqual(["claude_code", "codex", "workbuddy"]);
    expect(guideClients(res(), "key").map((c) => c.client)).toEqual(["claude_code", "codex", "workbuddy", "stdio_bridge"]);
    expect(guideClients(null, "key")).toEqual([]);
  });

  it("切换方式时保留仍可见的选择，否则落到第一个", () => {
    const oauth = guideClients(res(), "oauth");
    expect(pickClient(oauth, "codex")?.client).toBe("codex");
    expect(pickClient(oauth, "stdio_bridge")?.client).toBe("claude_code");
    expect(pickClient(oauth, null)?.client).toBe("claude_code");
    expect(pickClient([], "codex")).toBeNull();
  });

  it("取某方式的那一项；没有就是 null", () => {
    const stdio = res().items[0]!;
    expect(modeOf(stdio, "key")?.mode).toBe("key");
    expect(modeOf(stdio, "oauth")).toBeNull();
  });

  it("主视觉六个客户端，首字母兜底", () => {
    expect(HERO_CLIENTS).toEqual(["claude_code", "codex", "cursor", "gemini_cli", "opencode", "workbuddy"]);
    expect(clientInitial("workBuddy")).toBe("W");
    expect(clientInitial("  ")).toBe("?");
    expect(clientInitial(undefined)).toBe("?");
  });
});

describe("复制内容", () => {
  it("复制的就是服务端给的话术，原样不改", () => {
    const entry = modeOf(res().items[3]!, "oauth");
    expect(guideCopyText(entry)).toBe(entry!.agentPrompt);
  });

  it("密钥方式复制的内容保留占位符、不含明文", () => {
    for (const item of res().items) {
      const text = guideCopyText(modeOf(item, "key"));
      expect(text).toContain(PH);
      expect(text).not.toMatch(/sk_mcp_/);
    }
  });

  it("内容形似明文时拒绝复制", () => {
    expect(guideCopyText(mode("key", { agentPrompt: "Bearer sk_mcp_abcdef0123" }))).toBeNull();
  });

  it("不支持的方式不给复制内容，并给出说明", () => {
    const entry = modeOf(res().items[1]!, "oauth");
    expect(guideCopyText(entry)).toBeNull();
    expect(unsupportedText(entry)).toBe("待实测：请用密钥");
    expect(unsupportedText(null)).toContain("密钥");
  });
});
