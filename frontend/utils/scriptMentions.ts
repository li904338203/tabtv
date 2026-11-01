export type ScriptMentionType = "character" | "scene" | "prop" | "unknown";

export type ScriptMentionAsset = {
  id?: string;
  name: string;
  assetType?: string | null;
};

export type ScriptMentionSegment = {
  text: string;
  mention: boolean;
  name?: string;
  type?: ScriptMentionType;
  assetId?: string;
};

const CHARACTER_WORDS = ["主角", "配角", "人物", "角色", "主持人", "导演", "记者", "顾客", "老师", "学生", "医生", "老人", "孩子", "少年", "少女", "先生", "女士"];
const SCENE_WORDS = ["场景", "摄影棚", "片场", "舞台", "办公室", "会议室", "教室", "卧室", "客厅", "厨房", "街道", "公园", "广场", "森林", "海边", "山谷", "车站", "机场", "医院", "学校", "商店", "餐厅", "咖啡馆", "酒吧", "工厂", "仓库", "房间"];
const PROP_WORDS = ["道具", "摄像机", "相机", "电脑", "手机", "平板", "麦克风", "耳机", "键盘", "鼠标", "汽车", "车辆", "桌子", "椅子", "杯子", "箱子", "书本", "乐器", "灯具", "眼镜", "背包", "长剑", "短剑", "刀", "枪"];
const SCENE_CONTEXT = ["在", "位于", "进入", "走进", "来到", "抵达", "返回", "离开", "穿过"];
const PROP_CONTEXT = ["使用", "操作", "拿着", "手持", "握住", "佩戴", "打开", "举起", "端起", "驾驶"];

function escapeRegExp(value: string) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function normalizeType(value?: string | null): ScriptMentionType {
  if (value === "character" || value === "scene" || value === "prop") return value;
  return "unknown";
}

function explicitType(value?: string): ScriptMentionType | undefined {
  if (value === "角色") return "character";
  if (value === "场景") return "scene";
  if (value === "道具") return "prop";
  return undefined;
}

function includesAny(value: string, words: string[]) {
  return words.some(word => value.includes(word));
}

function endsWithAny(value: string, words: string[]) {
  return words.some(word => value.endsWith(word));
}

export function inferScriptMentionType(name: string, source: string, mentionStart: number): ScriptMentionType {
  if (includesAny(name, CHARACTER_WORDS)) return "character";
  if (includesAny(name, PROP_WORDS)) return "prop";
  if (includesAny(name, SCENE_WORDS)) return "scene";

  const prefix = source.slice(Math.max(0, mentionStart - 12), mentionStart).replace(/\s+/g, "");
  if (endsWithAny(prefix, PROP_CONTEXT)) return "prop";
  if (endsWithAny(prefix, SCENE_CONTEXT)) return "scene";
  return "character";
}

/**
 * 把描述切成「普通文字 / @提及」两种片段。
 *
 * @param entities 这一镜的实体（服务端 `shot.entities`）。描述里**不带 @ 的同名字样**也按提及高亮 ——
 *   分镜输出契约此前漏了「描述里写 @名称」这一条（CD-78），那期间生成的描述一个 @ 都没有，
 *   只认 @ 的话这些镜头的资产永远不高亮。只认**本镜自己的**实体，别处的同名字样不碰。
 */
export function segmentScriptMentions(
  source: string,
  assets: ScriptMentionAsset[] = [],
  entities: Array<{ name: string; type?: string | null }> = []
): ScriptMentionSegment[] {
  if (!source) return [];
  const uniqueAssets = Array.from(new Map(
    assets.filter(asset => asset.name.trim()).map(asset => [asset.name.trim().toLocaleLowerCase(), asset])
  ).values());
  const knownNames = uniqueAssets.map(asset => asset.name.trim()).sort((a, b) => b.length - a.length);
  const knownPattern = knownNames.length ? `${knownNames.map(escapeRegExp).join("|")}|` : "";
  const bareNames = Array.from(new Set(entities.map(entity => entity.name.trim()).filter(Boolean)))
    .sort((a, b) => b.length - a.length);
  const barePattern = bareNames.length ? `|(${bareNames.map(escapeRegExp).join("|")})` : "";
  const matcher = new RegExp(`@(?:(角色|场景|道具)[:：])?(${knownPattern}[\\p{L}\\p{N}_-]{1,80})${barePattern}`, "gu");
  const segments: ScriptMentionSegment[] = [];
  let cursor = 0;

  for (const match of source.matchAll(matcher)) {
    const start = match.index ?? 0;
    if (start > cursor) segments.push({ text: source.slice(cursor, start), mention: false });
    const bare = match[3];
    const name = bare ?? match[2] ?? "";
    const asset = uniqueAssets.find(item => item.name.trim().toLocaleLowerCase() === name.toLocaleLowerCase());
    const entityType = bare ? entities.find(entity => entity.name.trim() === bare)?.type : undefined;
    const type = explicitType(match[1])
      ?? (asset ? normalizeType(asset.assetType) : entityType ? normalizeType(entityType) : inferScriptMentionType(name, source, start));
    segments.push({ text: match[0], mention: true, name, type, assetId: asset?.id });
    cursor = start + match[0].length;
  }

  if (cursor < source.length) segments.push({ text: source.slice(cursor), mention: false });
  return segments;
}

/** 候选列表按这个顺序分组展示；unknown 兜底放最后 */
const MENTION_TYPE_ORDER: ScriptMentionType[] = ["character", "scene", "prop", "unknown"];

/**
 * 挑选 @ 候选：按类型分配名额，而不是整体截断。
 *
 * <p>资产是按 asset_type 排序取回的（character 在最前），一刀切前 N 个会让场景和道具
 * 一个都露不出来 —— 用户只能看到人物，以为不支持其他类型。
 */
export function pickMentionSuggestions(
  assets: ScriptMentionAsset[],
  keyword: string,
  perType = 6
): ScriptMentionAsset[] {
  const needle = keyword.trim().toLocaleLowerCase();
  const matched = assets.filter(asset => !needle || asset.name.toLocaleLowerCase().includes(needle));
  return MENTION_TYPE_ORDER.flatMap(type =>
    matched.filter(asset => normalizeType(asset.assetType) === type).slice(0, perType)
  );
}

export function scriptMentionTypeLabel(type: ScriptMentionType) {
  return ({ character: "人物", scene: "场景", prop: "道具", unknown: "引用" } as const)[type];
}
