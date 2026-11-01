import { describe, expect, it } from "vitest";
import {
  fillSnippet,
  orderSnippets,
  renderSnippets,
  snippetsEmptyText,
  snippetsUrlNotice,
  type McpClientSnippets,
} from "./mcpSnippets";

const PH = "<YOUR_MCP_KEY>";
const URL = "https://mcp.example.com/mcp";

function res(overrides: Partial<McpClientSnippets> = {}): McpClientSnippets {
  return {
    mcpUrl: URL,
    mcpUrlConfigured: true,
    keyPlaceholder: PH,
    toolsets: [],
    items: [
      { client: "stdio_bridge", label: "stdio 桥接", format: "json", location: "x", note: "", modes: [], content: `{"env":{"A":"Bearer ${PH}"},"url":"${URL}"}` },
      { client: "cursor", label: "Cursor", format: "json", location: "x", note: "", modes: [], content: `{"url":"${URL}","headers":{"Authorization":"Bearer ${PH}"}}` },
      { client: "claude_code", label: "Claude Code", format: "shell", location: "x", note: "", modes: [], content: `claude mcp add seeaitv '${URL}' --header 'Authorization: Bearer ${PH}'` },
    ],
    ...overrides,
  };
}

describe("占位符", () => {
  it("占位符替换为明文", () => {
    const key = "sk_mcp_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG";
    expect(fillSnippet(`a ${PH} b ${PH}`, PH, key)).toBe(`a ${key} b ${key}`);
    for (const item of renderSnippets(res(), key)) {
      expect(item.content).toContain(key);
      expect(item.content).not.toContain(PH);
    }
  });

  it("未创建密钥时保留占位符", () => {
    const original = res();
    for (const item of renderSnippets(original, null)) {
      expect(item.content).toContain(PH);
      expect(item.content).not.toContain("sk_mcp_");
      expect(original.items.some((o) => o.content === item.content)).toBe(true);
    }
    expect(fillSnippet(`x ${PH}`, PH, "")).toBe(`x ${PH}`);
  });

  it("替换不改动原对象", () => {
    const original = res();
    const before = JSON.stringify(original);
    renderSnippets(original, "sk_mcp_secret");
    expect(JSON.stringify(original)).toBe(before);
  });
});

describe("地址与内容", () => {
  it("各客户端URL取自服务端", () => {
    const other = "https://other.example.org/mcp";
    const r = res({
      mcpUrl: other,
      items: res().items.map((i) => ({ ...i, content: i.content.split(URL).join(other) })),
    });
    for (const item of renderSnippets(r, "sk_mcp_x")) {
      expect(item.content).toContain(other);
      expect(item.content).not.toContain("localhost");
      expect(item.content).not.toContain(URL);
    }
  });

  it("片段不含其他用户信息", () => {
    const key = "sk_mcp_mine";
    const out = renderSnippets(res(), key);
    // 除了占位符被换成本次的明文，其余内容与服务端给的逐字一致 —— 前端不往片段里加任何东西
    for (const item of out) {
      const server = res().items.find((i) => i.client === item.client)!;
      expect(item.content).toBe(server.content.split(PH).join(key));
      expect(item.content.match(/sk_mcp_\w+/g)?.every((m) => m === key) ?? true).toBe(true);
    }
  });

  it("按客户端排序，未知的排最后", () => {
    const items = [...res().items, { client: "zzz", label: "Z", format: "json" as const, location: "", note: "", modes: [], content: "" }];
    expect(orderSnippets(items).map((i) => i.client)).toEqual(["claude_code", "cursor", "stdio_bridge", "zzz"]);
  });
});

describe("空与未配置", () => {
  it("片段为空时给出说明", () => {
    expect(snippetsEmptyText(res({ items: [] }))).toBe("暂时没有可用的配置片段");
    expect(snippetsEmptyText(null)).not.toBeNull();
    expect(snippetsEmptyText(res())).toBeNull();
  });

  it("地址未配置时如实说明", () => {
    expect(snippetsUrlNotice(res())).toBeNull();
    expect(snippetsUrlNotice(res({ mcpUrlConfigured: false, mcpUrl: "<SEEAITV_MCP_URL>" }))).toContain("<SEEAITV_MCP_URL>");
  });
});
