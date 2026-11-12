/**
 * 设置 → 模型与厂商：整套模型配置的导出 / 导入（换电脑、重装时同步）。
 *
 * 文件由后端生成与解析（`/admin/model-config/export|import`）：要读出明文 Key，写入还有先后顺序，
 * 前端只负责选文件、展示预览与结果。这里是文案与汇总的纯函数。
 */

export interface ModelConfigItem {
  code: string;
  name: string;
  action: "create" | "update";
  status: string;
  hasKey?: boolean;
  ok: boolean;
  error?: string;
  warnings?: string[];
}

export interface ModelConfigReport {
  dryRun: boolean;
  problems: string[];
  providers: ModelConfigItem[];
  models: ModelConfigItem[];
  storage: "none" | "set";
  storageEndpoint?: string;
  localOnly: { providers: string[]; models: string[] };
  restartRequired: boolean;
}

export function modelConfigFileName(now = new Date()): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `tabtv-model-config-${now.getFullYear()}${p(now.getMonth() + 1)}${p(now.getDate())}.json`;
}

/** 读文件：只确认是 JSON 对象，形状交给后端校验（后端会说清楚哪里不对）。 */
export function parseModelConfigText(text: string): { ok: true; data: Record<string, unknown> } | { ok: false; error: string } {
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch {
    return { ok: false, error: "文件不是合法的 JSON" };
  }
  if (typeof data !== "object" || data === null || Array.isArray(data)) return { ok: false, error: "文件内容应是一个 JSON 对象" };
  const d = data as Record<string, unknown>;
  if (d.kind !== "model-config") return { ok: false, error: "这不是「模型配置」导出的文件（技能、提示词等请到各自页面导入）" };
  return { ok: true, data: d };
}

const count = (items: ModelConfigItem[], action: "create" | "update") => items.filter(i => i.action === action).length;

/** 预览的几行摘要。 */
export function modelConfigPreviewLines(r: ModelConfigReport): string[] {
  const lines = [
    `厂商 ${r.providers.length} 个：新建 ${count(r.providers, "create")}，覆盖 ${count(r.providers, "update")}`,
    `模型 ${r.models.length} 个：新建 ${count(r.models, "create")}，覆盖 ${count(r.models, "update")}`
  ];
  const keys = r.providers.filter(p => p.hasKey).length;
  lines.push(keys ? `包含 ${keys} 个厂商的 API Key，会写入本机` : "文件里没有 API Key，本机已有的 Key 保留不变");
  if (r.storage === "set") lines.push(`公网中转存储：${r.storageEndpoint ?? ""}（导入后需重启应用生效）`);
  const local = r.localOnly.providers.length + r.localOnly.models.length;
  if (local) lines.push(`本机另有 ${r.localOnly.providers.length} 个厂商、${r.localOnly.models.length} 个模型不在文件里，保持不动`);
  return lines;
}

export interface ModelConfigOutcome {
  ok: number;
  failed: Array<{ label: string; message: string }>;
  warned: Array<{ label: string; message: string }>;
}

/** 导入结果：成功数、失败项、有提醒的项。 */
export function modelConfigOutcome(r: ModelConfigReport): ModelConfigOutcome {
  const out: ModelConfigOutcome = { ok: 0, failed: [], warned: [] };
  const walk = (kind: string, items: ModelConfigItem[]) => {
    for (const it of items) {
      const label = `${kind} ${it.name || it.code}`;
      if (it.ok) out.ok++;
      else out.failed.push({ label, message: it.error || "未知原因" });
      for (const w of it.warnings ?? []) out.warned.push({ label, message: w });
    }
  };
  walk("厂商", r.providers);
  walk("模型", r.models);
  for (const p of r.problems) out.failed.push({ label: "配置", message: p });
  return out;
}
