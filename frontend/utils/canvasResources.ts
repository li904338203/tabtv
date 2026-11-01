/**
 * 画布「添加节点」菜单与「添加资源」（上传 / 从生成历史选择）的纯函数。
 *
 * - 菜单只列能直接建出来用的节点：组（由脚本建）、风格 / 特效（工具栏的风格广场、特效库）、文档不在菜单里（用户反馈 2026-10-29）；
 *   次序照参考产品：文本 → 脚本 → 图片 → 视频 → 导演台 → 智能剪辑 → 音频，后台新加的类型排在最后（配置可以比前端新）。
 * - 资源建成的节点是「已有产出」形态：只带 `mediaId`，**不带生成 ID** —— 生成 ID 只能在它自己的项目里当输入（GAP-16），
 *   从别的项目的历史里挑来的产出按媒体声明参考，与导演台 / 智能剪辑发布到画布同一形态。
 */
import type { GenerationView } from "~/types/canvas";

export type ResourceMediaType = "image" | "video" | "audio";

const HIDDEN_NODE_TYPES = new Set(["group", "style", "effect", "document"]);
const NODE_ORDER = ["text", "script", "image", "video", "director", "smartcut", "audio"];

/** 加节点菜单里列哪些节点、按什么顺序。 */
export function canvasAddMenuTypes<T extends { code?: string }>(types: readonly T[] | undefined): T[] {
  const rank = (code?: string) => {
    const index = NODE_ORDER.indexOf(code ?? "");
    return index < 0 ? NODE_ORDER.length : index;
  };
  return (types ?? [])
    .map((type, index) => ({ type, index }))
    .filter(({ type }) => type.code && !HIDDEN_NODE_TYPES.has(type.code))
    .sort((a, b) => rank(a.type.code) - rank(b.type.code) || a.index - b.index)
    .map(({ type }) => type);
}

/** 菜单行尾的小标签：只有导演台挂「3D」（它是 3D 场景，不是生成节点），其余不挂。 */
export function canvasAddMenuBadge(code?: string): string {
  return code === "director" ? "3D" : "";
}

/** 「已有产出」形态的节点产出（没有生成 ID）。 */
export function existingOutputGeneration(type: ResourceMediaType, mediaId: string): GenerationView {
  return { status: "success", progress: 100, outputType: type, mediaId, outputUrl: `/api/v1/media/${mediaId}` };
}

export type HistoryOutput = {
  key: string;
  generationId: string;
  projectId: string;
  index: number;
  type: ResourceMediaType;
  mediaId: string;
  modelCode?: string;
  operation?: string;
  createdAt: string;
};

type HistoryGeneration = {
  id: string;
  projectId: string;
  status: string;
  operation?: string;
  modelCode?: string;
  createdAt: string;
  outputs?: Array<{ index: number; type: string; mediaId?: string }>;
};

/**
 * 生成历史里能拿来建节点的产物：成功的、媒体类（图 / 视频 / 音频）、有媒体 ID 的 —— **每个产物一项**，
 * 一次出四张图就是四项（不隐式只给 outputs[0]，GAP-12）。服务端列表不按状态过滤，这里过滤。
 */
export function historyOutputs(items: readonly HistoryGeneration[], type: ResourceMediaType | "all" = "all"): HistoryOutput[] {
  const out: HistoryOutput[] = [];
  for (const g of items) {
    if (g.status !== "success") continue;
    for (const o of g.outputs ?? []) {
      if (o.type !== "image" && o.type !== "video" && o.type !== "audio") continue;
      if (!o.mediaId || (type !== "all" && o.type !== type)) continue;
      out.push({
        key: `${g.id}:${o.index}`, generationId: g.id, projectId: g.projectId, index: o.index, type: o.type,
        mediaId: o.mediaId, modelCode: g.modelCode, operation: g.operation, createdAt: g.createdAt
      });
    }
  }
  return out;
}

/** 一批资源节点的落点：第一个用画布给的默认位置，其余在它右边排开（每行 3 个）。 */
export function resourceNodePositions(origin: { x: number; y: number }, count: number): Array<{ x: number; y: number }> {
  return Array.from({ length: count }, (_, index) => ({ x: origin.x + (index % 3) * 430, y: origin.y + Math.floor(index / 3) * 380 }));
}

/** 资源节点的标题：上传用文件名；历史用「类型 · 模型」。 */
export function historyNodeTitle(item: Pick<HistoryOutput, "type" | "modelCode">): string {
  const label = item.type === "image" ? "图片" : item.type === "video" ? "视频" : "音频";
  return item.modelCode ? `${label} · ${item.modelCode}` : label;
}
