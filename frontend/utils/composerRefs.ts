/**
 * Agent 输入框的引用胶囊（AGT-29，设计 03 §2.3、开发 03 §5「选中即引用」）。纯函数。
 *
 * 引用以**胶囊对象**为准（`{kind, id, label, auto}`），不靠解析正文里的 `@文字`：
 * 正文可以随便改，胶囊才是用户明确选了的东西。
 *
 * ⚠️ 两条「错了不报错」的：
 *  1. 取消选中**只移除自动加入的**胶囊；用户手动 `@` 的保留 —— 否则用户点一下画布空白处，
 *     刚精心挑的引用全没了，而他不会想到是「点空白」导致的。
 *  2. 发给服务端的 `refs` **只含 `{kind,id}`**（AG-03、GO-27）：缩略图地址、标题都只是展示，
 *     带上 URL 的引用服务端一律 400；归属由服务端按 ID 校验。
 */
export type ComposerRefKind = "node" | "media" | "asset";

export type ComposerRef = {
  kind: ComposerRefKind;
  id: string;
  /** 胶囊上显示的名字。 */
  label: string;
  /** 由「画布选中」自动加入；取消选中时随之移除。手动 `@` 的为 false。 */
  auto: boolean;
  /** 展示用的媒体 ID（缩略图）；**不发给服务端**。 */
  thumbMediaId?: string;
  /** 展示用的类型提示（image / video / audio / text / character …）。 */
  hint?: string;
  /** 营销槽位（product / character / source_video，MKT-T-12）；普通附件与 `@` 引用不带。 */
  slot?: string;
  /** 槽内顺序（从 1 起，商品 order=1 即主图）；只与 `slot` 一起出现。 */
  order?: number;
};

/** 发给服务端的引用：`{kind,id}`，营销槽位另带 `slot` / `order`。 */
export type SerializedRef = { kind: ComposerRefKind; id: string; slot?: string; order?: number };

export type NodeRefSource = { id: string; label: string; thumbMediaId?: string; hint?: string };

function sameRef(a: Pick<ComposerRef, "kind" | "id">, b: Pick<ComposerRef, "kind" | "id">): boolean {
  return a.kind === b.kind && a.id === b.id;
}

/**
 * 画布选中变化 → 胶囊。
 *
 * - 手动胶囊一律保留；
 * - 自动胶囊只保留仍被选中的；
 * - 新选中的节点追加为自动胶囊（已经有同一个节点的胶囊 —— 无论手动还是自动 —— 就不重复加）；
 * - 查不到的节点 ID（已被删、或是画框）不加。
 */
export function syncSelectedRefs(
  refs: ComposerRef[],
  selectedIds: readonly string[],
  lookup: (id: string) => NodeRefSource | undefined
): ComposerRef[] {
  const selected = new Set(selectedIds);
  const kept = refs.filter(ref => !ref.auto || (ref.kind === "node" && selected.has(ref.id)));
  for (const id of selectedIds) {
    if (kept.some(ref => ref.kind === "node" && ref.id === id)) continue;
    const source = lookup(id);
    if (!source) continue;
    kept.push({ kind: "node", id, label: source.label, auto: true, thumbMediaId: source.thumbMediaId, hint: source.hint });
  }
  return kept;
}

/**
 * 手动加一个引用（`@` 选中、上传完成）。已有同一个自动胶囊时**转成手动**（之后取消选中不会把它带走）；
 * 已有手动的不重复加。
 */
export function addManualRef(refs: ComposerRef[], ref: Omit<ComposerRef, "auto">): ComposerRef[] {
  const index = refs.findIndex(item => sameRef(item, ref));
  if (index < 0) return [...refs, { ...ref, auto: false }];
  if (!refs[index]!.auto) return refs;
  return refs.map((item, i) => (i === index ? { ...item, auto: false } : item));
}

export function removeRef(refs: ComposerRef[], target: Pick<ComposerRef, "kind" | "id">): ComposerRef[] {
  return refs.filter(item => !sameRef(item, target));
}

/**
 * 发给服务端的 `refs`：只有 `{kind,id}`，去重，保持胶囊顺序。
 * 带 `slot` 的（营销槽位）另带 `slot` 与 `order`，去重按 slot 分开算；不带 slot 的输出与原来逐字一致。
 */
export function serializeRefs(refs: ComposerRef[]): SerializedRef[] {
  const seen = new Set<string>();
  const out: SerializedRef[] = [];
  for (const ref of refs) {
    const key = ref.slot ? `${ref.slot}|${ref.kind}:${ref.id}` : `${ref.kind}:${ref.id}`;
    if (!ref.id || seen.has(key)) continue;
    seen.add(key);
    if (ref.slot) out.push({ kind: ref.kind, id: ref.id, slot: ref.slot, ...(ref.order !== undefined ? { order: ref.order } : {}) });
    else out.push({ kind: ref.kind, id: ref.id });
  }
  return out;
}

/** 画布节点 → 引用来源。画框（纯视觉容器）不能被引用。 */
export function nodeRefSource(node: {
  id: string;
  data?: { nodeType?: string; title?: string; generation?: { mediaId?: string } };
}): NodeRefSource | undefined {
  const type = node.data?.nodeType ?? "";
  if (!node.id || type === "frame") return undefined;
  const title = node.data?.title?.trim();
  return {
    id: node.id,
    label: title || NODE_TYPE_LABELS[type] || "节点",
    thumbMediaId: node.data?.generation?.mediaId || undefined,
    hint: type || undefined
  };
}

const NODE_TYPE_LABELS: Record<string, string> = {
  text: "文本节点",
  image: "图片节点",
  video: "视频节点",
  audio: "音频节点",
  script: "脚本节点",
  document: "文档",
  group: "组节点"
};

/**
 * `@` 候选的第一组：本画布的节点，**按最近创建**（画布节点按创建顺序追加，倒序即最近在前），
 * 按关键字过滤标题。
 */
export function nodeCandidates<T extends { id: string; data?: { nodeType?: string; title?: string; generation?: { mediaId?: string } } }>(
  nodes: readonly T[],
  query: string,
  limit = 20
): NodeRefSource[] {
  const keyword = query.trim().toLowerCase();
  const out: NodeRefSource[] = [];
  for (let i = nodes.length - 1; i >= 0 && out.length < limit; i -= 1) {
    const source = nodeRefSource(nodes[i]!);
    if (!source) continue;
    if (keyword && !source.label.toLowerCase().includes(keyword)) continue;
    out.push(source);
  }
  return out;
}
