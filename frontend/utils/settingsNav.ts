/**
 * 设置页的分区（TT-T-17，dev/03 §3）。新增分区只改这里：侧栏导航与「缺模型」跳转锚点共用这一份。
 */
export interface SettingsSection {
  key: string;
  path: string;
  label: string;
  hint: string;
}

export const SETTINGS_SECTIONS: SettingsSection[] = [
  { key: "models", path: "/settings/models", label: "模型与厂商", hint: "填写 API Key、启用模型" },
  { key: "skills", path: "/settings/skills", label: "Agent 技能", hint: "技能正文与附属文档" },
  { key: "prompts", path: "/settings/prompts", label: "提示词", hint: "内置提示词模板" },
  { key: "presets", path: "/settings/presets", label: "图片预设", hint: "图片工具的配方" },
  { key: "marketing", path: "/settings/marketing", label: "营销", hint: "创意 / Hook / 风格与预置角色" },
  { key: "general", path: "/settings/general", label: "常规", hint: "并发、水印、数据目录" },
  { key: "storage", path: "/settings/storage", label: "存储", hint: "占用与可选 S3" },
  { key: "diagnostics", path: "/settings/diagnostics", label: "环境自检", hint: "ffmpeg、数据库、磁盘" },
  { key: "logs", path: "/settings/logs", label: "调用日志", hint: "发给模型上游的请求" },
  { key: "about", path: "/settings/about", label: "关于", hint: "版本、更新、许可证" }
];

/** 当前路径属于哪个分区（子路径也算）。 */
export function activeSettingsSection(path: string): string {
  const hit = SETTINGS_SECTIONS.find(s => path === s.path || path.startsWith(`${s.path}/`));
  return hit?.key ?? "";
}

/** 字节数的人话（设置 → 存储）。 */
export function formatBytes(n: number | null | undefined): string {
  const v = Number(n ?? 0);
  if (!Number.isFinite(v) || v <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let x = v;
  while (x >= 1024 && i < units.length - 1) {
    x /= 1024;
    i++;
  }
  return `${x >= 100 || i === 0 ? Math.round(x) : x.toFixed(1)} ${units[i]}`;
}
