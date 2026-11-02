/**
 * 设置页的 JSON 导入 / 导出（TT-T-19~21）：文件形状校验与错误文案。
 *
 * 文件形状：`{ format: "tabtv.settings", kind, version: 1, exportedAt, items: [...] }`。
 * ⚠️ `kind` 必须对得上：把技能文件导进预设页，字段名碰巧重叠的部分会被当成预设存进去，
 *    而那不报错 —— 只会多出一批谁都不认识的预设。
 * ⚠️ 同一个文件里 key 重复要拒绝，不悄悄取后一个：那样导入的结果取决于文件里的顺序，
 *    而使用者看到「导入成功 N 条」会以为 N 条都进去了。
 * ⚠️ 导入按 key 覆盖已有条目（技能 / 预设 / 选项按 code，角色按名称）；覆盖一律走正常的保存接口，
 *    不绕过服务端校验。
 */
export const TRANSFER_FORMAT = "tabtv.settings";
export const TRANSFER_VERSION = 1;

export type TransferKind = "skills" | "prompts" | "presets" | "marketing-options" | "marketing-characters";

export const KIND_LABELS: Record<TransferKind, string> = {
  skills: "Agent 技能",
  prompts: "提示词",
  presets: "图片预设",
  "marketing-options": "营销选项",
  "marketing-characters": "营销角色"
};

/** 每种文件的条目主键。 */
export const KIND_KEYS: Record<TransferKind, string> = {
  skills: "code",
  prompts: "code",
  presets: "code",
  "marketing-options": "code",
  "marketing-characters": "name"
};

/**
 * 条目的唯一键。提示词按「用途 + 模型」：同一个用途可以有通用版与多个模型专属版。
 */
export function transferItemKey(kind: TransferKind, item: Record<string, unknown>): string {
  const v = (k: string) => (typeof item[k] === "string" ? (item[k] as string).trim() : "");
  if (kind === "prompts") return v("modelCode") ? `${v("code")}/${v("modelCode")}` : v("code");
  return v(KIND_KEYS[kind]);
}

export interface TransferFile<T = Record<string, unknown>> {
  format: string;
  kind: TransferKind;
  version: number;
  exportedAt: string;
  items: T[];
}

export function buildExport<T>(kind: TransferKind, items: T[], now = new Date()): TransferFile<T> {
  return { format: TRANSFER_FORMAT, kind, version: TRANSFER_VERSION, exportedAt: now.toISOString(), items };
}

export type ParseResult =
  | { ok: true; items: Record<string, unknown>[] }
  | { ok: false; error: string };

const isObj = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);

/** 校验导入文件；第一条问题即返回（文案说清是第几项、缺什么）。 */
export function parseImport(text: string, kind: TransferKind): ParseResult {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return { ok: false, error: "不是合法的 JSON 文件" };
  }
  if (!isObj(raw) || raw.format !== TRANSFER_FORMAT) return { ok: false, error: "不是 tabtv 设置导出的文件（缺少 format 标记）" };
  if (raw.kind !== kind) {
    const other = KIND_LABELS[raw.kind as TransferKind];
    return { ok: false, error: other ? `这是「${other}」的导出文件，不能导入到「${KIND_LABELS[kind]}」` : `文件类型「${String(raw.kind)}」认不出` };
  }
  if (typeof raw.version !== "number" || raw.version > TRANSFER_VERSION) {
    return { ok: false, error: `文件版本 ${String(raw.version)} 比本应用新，请先升级应用` };
  }
  if (!Array.isArray(raw.items)) return { ok: false, error: "文件里没有 items 列表" };
  if (!raw.items.length) return { ok: false, error: "文件里一条都没有" };
  const key = KIND_KEYS[kind];
  const seen = new Set<string>();
  const items: Record<string, unknown>[] = [];
  for (const [i, it] of raw.items.entries()) {
    if (!isObj(it)) return { ok: false, error: `第 ${i + 1} 项不是对象` };
    if (!(typeof it[key] === "string" && (it[key] as string).trim())) return { ok: false, error: `第 ${i + 1} 项缺少 ${key}` };
    const k = transferItemKey(kind, it);
    if (seen.has(k)) return { ok: false, error: `第 ${i + 1} 项的 ${key}「${k}」与前面重复` };
    seen.add(k);
    items.push(it);
  }
  return { ok: true, items };
}

export interface ImportPlan<T, E> {
  creates: T[];
  updates: Array<{ item: T; current: E }>;
}

/** 按 key 分成「新建」与「覆盖已有」两组。 */
export function planImport<T extends Record<string, unknown>, E>(
  items: readonly T[], existing: readonly E[], keyOf: (e: E) => string, key: string
): ImportPlan<T, E> {
  const byKey = new Map(existing.map(e => [keyOf(e), e]));
  const plan: ImportPlan<T, E> = { creates: [], updates: [] };
  for (const item of items) {
    const current = byKey.get(String(item[key]).trim());
    if (current) plan.updates.push({ item, current });
    else plan.creates.push(item);
  }
  return plan;
}

/** 导入结果的一句话（失败逐条列出，不吞掉）。 */
export function importSummary(created: number, updated: number, failures: Array<{ key: string; message: string }>): string {
  const head = `新建 ${created} 条，覆盖 ${updated} 条`;
  if (!failures.length) return head;
  return `${head}；${failures.length} 条失败：${failures.map(f => `${f.key}（${f.message}）`).join("；")}`;
}

/** 导出文件名：`tabtv-<kind>-YYYYMMDD.json`。 */
export function exportFileName(kind: TransferKind, now = new Date()): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `tabtv-${kind}-${now.getFullYear()}${p(now.getMonth() + 1)}${p(now.getDate())}.json`;
}

/** 浏览器里下载一份 JSON（没有测试：只碰 DOM）。 */
export function downloadJson(name: string, data: unknown) {
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  // ⚠️ 不能同步 revoke：部分浏览器 click 之后才异步取这个地址，立刻撤销会下出 0 字节文件。
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/** 读用户选的文件为文本。 */
export function readFileText(file: File): Promise<string> {
  return file.text();
}
