/**
 * 「看看 seeAitv MCP 能做什么」的静态示意（`/mcp` 页）。
 *
 * ⚠️ **写死的示意，不请求任何数据接口**：这是说明书不是演示环境。工具名取自 `api/mcp/tools.json`，
 *    用 Vitest 对照 tools.json 钉住「只出现真实存在的工具」与「每个花积分的调用之前都有一次确认」——
 *    示意里把顺序写反，等于教用户的 Agent 先花钱再说。示意里的金额只是举例，不是定价。
 */

export type ShowcaseKey = "generate" | "canvas" | "film";

export interface ShowcaseCall {
  /** MCP 工具名（`工具` 或 `工具.动作`）。 */
  tool: string;
  detail: string;
  /** 这一步花积分（展示用文案，如「✦12」）。 */
  cost?: string;
}

export type ShowcaseStep =
  | { kind: "call"; call: ShowcaseCall }
  | { kind: "confirm"; text: string };

export interface ShowcaseDemo {
  key: ShowcaseKey;
  label: string;
  user: string;
  steps: ShowcaseStep[];
  reply: string;
  result: { title: string; caption: string; items: string[] };
}

export const SHOWCASE_DEMOS: readonly ShowcaseDemo[] = [
  {
    key: "generate",
    label: "全模型调用生成",
    user: "用 seeaitv 给「夏日汽水」出 4 张竖版海报草图，3:4，清爽的蓝色调。",
    steps: [
      { kind: "call", call: { tool: "whoami", detail: "个人工作区 · 余额 ✦500" } },
      { kind: "call", call: { tool: "models.list", detail: "筛出当前可用的图片模型" } },
      { kind: "call", call: { tool: "quote", detail: "4 张 × ✦3 = ✦12（只报价，不扣费）" } },
      { kind: "confirm", text: "确认，按 ✦12 生成" },
      { kind: "call", call: { tool: "generate", detail: "带报价单号受理，扣费 ✦12", cost: "✦12" } },
      { kind: "call", call: { tool: "tasks.wait", detail: "4 张全部完成" } },
      { kind: "call", call: { tool: "media.get", detail: "取回产物与缩略图" } },
    ],
    reply: "4 张海报草图已生成，放在「夏日汽水」项目的画布里，本次消耗 ✦12。要不要挑一张做成短视频？",
    result: { title: "夏日汽水 · 海报草图", caption: "4 张 · 3:4 · 已写入画布", items: ["草图 1", "草图 2", "草图 3", "草图 4"] },
  },
  {
    key: "canvas",
    label: "读写画布与素材",
    user: "打开「品牌短片」，加一个文本节点写三句卖点，再把素材库里的产品图连到视频节点上。",
    steps: [
      { kind: "call", call: { tool: "projects.list", detail: "找到「品牌短片」" } },
      { kind: "call", call: { tool: "canvas_read", detail: "读取画布大纲：23 个节点" } },
      { kind: "call", call: { tool: "media.get", detail: "从素材库取出产品图" } },
      { kind: "call", call: { tool: "canvas_apply", detail: "新增 1 个文本节点、2 条连线（读写画布不扣积分）" } },
    ],
    reply: "已加好「卖点文案」节点并连上产品图，你打开画布就能看到，新节点带着客户端角标。",
    result: { title: "品牌短片 · 画布", caption: "新增 1 个节点 · 2 条连线", items: ["产品图", "卖点文案", "视频"] },
  },
  {
    key: "film",
    label: "脚本到成片",
    user: "把这段 30 秒的剧情做成分镜，出角色和场景资产，再生成视频组并渲染成片。",
    steps: [
      { kind: "call", call: { tool: "script.create", detail: "在项目里建脚本节点" } },
      { kind: "confirm", text: "分镜 ✦6，确认" },
      { kind: "call", call: { tool: "script.storyboard", detail: "拆成 6 个镜头", cost: "✦6" } },
      { kind: "confirm", text: "资产设定图 4 张 ✦12，确认" },
      { kind: "call", call: { tool: "script.assets", detail: "2 个角色、2 个场景出设定图", cost: "✦12" } },
      { kind: "confirm", text: "视频组 6 条 ✦60，确认" },
      { kind: "call", call: { tool: "script.group_execute", detail: "6 条视频排队生成", cost: "✦60" } },
      { kind: "call", call: { tool: "timeline.assemble", detail: "按分镜顺序装配时间线" } },
      { kind: "confirm", text: "渲染 ✦30，确认" },
      { kind: "call", call: { tool: "timeline.render", detail: "渲染 1080p 成片", cost: "✦30" } },
    ],
    reply: "成片已渲染完成（00:30 · 1080p），6 个镜头全部成功，已放进你的作品库待审核。",
    result: { title: "雨夜便利店 · 成片", caption: "6 镜 · 00:30 · 1080p", items: ["镜 1", "镜 2", "镜 3", "镜 4", "镜 5", "镜 6"] },
  },
];

/** 按 key 取示意；未知 key 落到第一个。 */
export function showcaseOf(key: string | null | undefined): ShowcaseDemo {
  return SHOWCASE_DEMOS.find((d) => d.key === key) ?? SHOWCASE_DEMOS[0]!;
}
