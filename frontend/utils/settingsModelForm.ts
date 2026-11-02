/**
 * 设置 → 模型与厂商的纯规则（TT-T-18）。从来源管理端 `utils/modelForm.ts` + `utils/secret.ts` 迁入，去掉会员门槛。
 *
 * 规则与服务端对应（`catalog/domain/admin_fields.go`、`ValidateModelProtocol`、`SaveProvider`、`ValidateParamSupport`），
 * 前端先拦一道是为了**在点保存之前**说清楚；服务端那道才是边界。
 *
 * ⚠️ 导出一律带 `settings` 前缀：Nuxt 自动导入 utils 的全部导出，重名时悄悄顶替。
 */
type Row = Record<string, unknown>;
const str = (v: unknown) => (typeof v === "string" ? v : "");
const num = (v: unknown, d = 0) => (typeof v === "number" && Number.isFinite(v) ? v : d);
const chars = (s: string) => [...s].length;

// ---------------------------------------------------------------- 密钥三态

/**
 * 密钥字段的三态（保留 / 替换 / 清除）。服务端**不回显密钥、连掩码都不回**，只回「配没配」。
 * 「保留」时请求体里**不带**这个字段；「替换」必须填真实值，看起来像掩码的当场拦下；「清除」只带 action。
 */
export type SettingsSecretAction = "keep" | "replace" | "clear";

export const settingsLooksMasked = (s: string) => /[*•]{4,}/.test(s);

export function settingsSecretPayload(action: SettingsSecretAction, value: string): { action: "replace"; value: string } | { action: "clear" } | undefined {
  if (action === "replace" && value.trim()) return { action: "replace", value: value.trim() };
  if (action === "clear") return { action: "clear" };
  return undefined;
}

// ---------------------------------------------------------------- 模型

export const SETTINGS_CODE_RE = /^[a-z0-9][a-z0-9_-]*$/;
export const SETTINGS_MODEL_DOMAINS = [
  { value: "image", label: "图片" },
  { value: "video", label: "视频" },
  { value: "text", label: "文本" },
  { value: "audio", label: "音频" }
] as const;

export interface SettingsProtocol {
  code: string;
  name: string;
  isAsync: boolean;
  requiresQueryPath: boolean;
  /** 能做多轮 + 工具调用（可作 Agent 推理模型）。 */
  chatCapable?: boolean;
}

export interface SettingsModelDraft {
  code: string;
  name: string;
  providerCode: string;
  domain: string;
  subType: string;
  upstreamModelCode: string;
  apiProtocol: string;
  submitPath: string;
  queryPath: string;
  estSeconds: number | null;
  maxInputChars: number | null;
  sort: number | null;
  status: "enabled" | "disabled";
  maxDurationSec: number | null;
  isMultiShot: boolean;
  hasNativeAudio: boolean;
  agentCapable: boolean;
}

/** 单次最长时长的上限（与服务端 `domain.MaxVideoDurationSec` 一致）。 */
export const SETTINGS_MAX_VIDEO_DURATION_SEC = 600;

export function settingsModelDraft(row: Row | null): SettingsModelDraft {
  if (!row) {
    return {
      code: "", name: "", providerCode: "", domain: "image", subType: "", upstreamModelCode: "",
      apiProtocol: "", submitPath: "", queryPath: "", estSeconds: 30, maxInputChars: null, sort: 100,
      status: "disabled", maxDurationSec: null, isMultiShot: false, hasNativeAudio: false, agentCapable: false
    };
  }
  return {
    code: str(row.code), name: str(row.name), providerCode: str(row.providerCode), domain: str(row.domain),
    subType: str(row.subType), upstreamModelCode: str(row.upstreamModelCode), apiProtocol: str(row.apiProtocol),
    submitPath: str(row.submitPath), queryPath: str(row.queryPath), estSeconds: num(row.estSeconds, 30),
    maxInputChars: typeof row.maxInputChars === "number" ? row.maxInputChars : null, sort: num(row.sort),
    status: row.status === "enabled" ? "enabled" : "disabled",
    maxDurationSec: typeof row.maxDurationSec === "number" ? row.maxDurationSec : null,
    isMultiShot: row.isMultiShot === true, hasNativeAudio: row.hasNativeAudio === true,
    agentCapable: row.agentCapable === true
  };
}

export function settingsValidateModel(d: SettingsModelDraft, models: Row[], protocols: SettingsProtocol[], editing: boolean): string {
  const code = d.code.trim();
  if (!SETTINGS_CODE_RE.test(code) || code.length > 64) return "模型编码只能用小写字母、数字、下划线与连字符，以字母或数字开头，最长 64 位";
  if (!editing && models.some(m => m.code === code)) return `编码「${code}」已存在`;
  if (!d.name.trim()) return "请填写模型名称";
  if (chars(d.name.trim()) > 64) return "模型名称不能超过 64 个字";
  if (!d.providerCode) return "请选择厂商";
  if (!SETTINGS_MODEL_DOMAINS.some(x => x.value === d.domain)) return "请选择模型类型";
  if (!d.upstreamModelCode.trim()) return "请填写上游模型名";
  const p = protocols.find(x => x.code === d.apiProtocol);
  if (!p) return "请选择 API 协议";
  if (!d.submitPath.trim()) return "请填写提交路径";
  // ⚠️ 异步协议缺查询路径：能存能启用，直到拿到任务号要轮询才发现没地方查
  if (p.requiresQueryPath && !d.queryPath.trim()) return `「${p.name}」是异步协议，必须填任务查询路径`;
  if (!p.isAsync && d.queryPath.trim()) return `「${p.name}」是同步协议，不应填查询路径`;
  if (d.estSeconds === null || !Number.isInteger(d.estSeconds) || d.estSeconds < 1 || d.estSeconds > 3600) {
    return "预计耗时必须是 1~3600 秒的整数（它决定超时：max(30, 预计耗时 × 3)）";
  }
  if (d.maxInputChars !== null && (!Number.isInteger(d.maxInputChars) || d.maxInputChars < 1)) return "输入字数上限必须是正整数，留空表示不限";
  if (d.sort === null || !Number.isInteger(d.sort)) return "排序必须是整数";
  if (d.domain === "video" && d.maxDurationSec !== null
    && (!Number.isInteger(d.maxDurationSec) || d.maxDurationSec < 1 || d.maxDurationSec > SETTINGS_MAX_VIDEO_DURATION_SEC)) {
    return `单次最长时长必须是 1~${SETTINGS_MAX_VIDEO_DURATION_SEC} 秒的整数，留空表示未配置`;
  }
  if (d.domain === "text" && d.agentCapable && p.chatCapable === false) {
    return `「${p.name}」不支持多轮对话与工具调用，不能作推理模型（请改用 OpenAI Chat 或 Anthropic Messages 协议）`;
  }
  return "";
}

/** 「支持 Agent」能不能勾：文本模型且协议能多轮 + 工具调用。协议还没选时不拦（以服务端为准）。 */
export function settingsAgentCapableAllowed(d: SettingsModelDraft, protocols: SettingsProtocol[]): boolean {
  if (d.domain !== "text") return false;
  const p = protocols.find(x => x.code === d.apiProtocol);
  return !p || p.chatCapable !== false;
}

/**
 * 请求体。⚠️ 保存是**整条替换**：表单里没有的字段不带就会被清空。
 * 会员门槛（来源的 minMemberLevelId）桌面版没有：不带这个字段，服务端按「保留」处理。
 */
export function settingsModelPayload(d: SettingsModelDraft) {
  const body: Record<string, unknown> = {
    code: d.code.trim(), name: d.name.trim(), providerCode: d.providerCode, domain: d.domain,
    subType: d.domain === "audio" ? d.subType : "", upstreamModelCode: d.upstreamModelCode.trim(),
    apiProtocol: d.apiProtocol, submitPath: d.submitPath.trim(), queryPath: d.queryPath.trim(),
    estSeconds: d.estSeconds, maxInputChars: d.maxInputChars, sort: d.sort ?? 0, status: d.status
  };
  // 视频能力：只有视频模型才带。时长 null 要**显式带上**（= 清除）；不带等于「保留」。
  if (d.domain === "video") {
    body.maxDurationSec = d.maxDurationSec;
    body.isMultiShot = d.isMultiShot;
    body.hasNativeAudio = d.hasNativeAudio;
  }
  body.agentCapable = d.domain === "text" && d.agentCapable;
  return body;
}

/** 已有历史任务后锁定的字段（服务端同样拒绝）。 */
export const SETTINGS_LOCKED_AFTER_HISTORY = ["providerCode", "apiProtocol", "domain", "upstreamModelCode"] as const;

// ---------------------------------------------------------------- 厂商

export type SettingsAuthType = "none" | "bearer" | "header" | "volc_v4";
const AUTH_TYPES: readonly string[] = ["none", "bearer", "header", "volc_v4"];

/** 火山 V4 签名的地域与服务：存在自定义请求头的两个约定键里，不随请求发出。 */
export const SETTINGS_VOLC_REGION_HEADER = "X-Volc-Region";
export const SETTINGS_VOLC_SERVICE_HEADER = "X-Volc-Service";
const isVolcKey = (name: string) =>
  [SETTINGS_VOLC_REGION_HEADER, SETTINGS_VOLC_SERVICE_HEADER].some(k => k.toLowerCase() === name.trim().toLowerCase());

export interface SettingsProviderDraft {
  code: string;
  name: string;
  adapterType: string;
  baseUrl: string;
  authType: SettingsAuthType;
  keyAction: SettingsSecretAction;
  /** 只在「替换」时有值；**从不回填**。 */
  keyValue: string;
  /** `volc_v4` 的两段密钥，保存时拼成 `AK:SK`。 */
  keyAk: string;
  keySk: string;
  volcRegion: string;
  volcService: string;
  headers: Array<{ name: string; value: string }>;
  testPath: string;
  connectTimeoutSec: number | null;
  requestTimeoutSec: number | null;
  idleTimeoutSec: number | null;
  maxRetries: number | null;
  maxConcurrent: number | null;
  status: "enabled" | "disabled";
  version: number;
  hasApiKey: boolean;
}

export function settingsProviderDraft(row: Row | null): SettingsProviderDraft {
  const all = row && row.headers && typeof row.headers === "object"
    ? Object.entries(row.headers as Record<string, unknown>).map(([name, value]) => ({ name, value: String(value) }))
    : [];
  const pick = (key: string) => all.find(h => h.name.trim().toLowerCase() === key.toLowerCase())?.value.trim() ?? "";
  const headers = all.filter(h => !isVolcKey(h.name));
  const sec = (v: unknown, d: number) => Math.max(1, Math.round(num(v, d * 1000) / 1000));
  return {
    code: str(row?.code), name: str(row?.name), adapterType: str(row?.adapterType) || "configurable_http",
    baseUrl: str(row?.baseUrl), authType: AUTH_TYPES.includes(str(row?.authType)) ? str(row?.authType) as SettingsAuthType : "bearer",
    keyAction: row ? "keep" : "replace", keyValue: "", keyAk: "", keySk: "",
    volcRegion: pick(SETTINGS_VOLC_REGION_HEADER), volcService: pick(SETTINGS_VOLC_SERVICE_HEADER),
    headers, testPath: str(row?.testPath),
    connectTimeoutSec: sec(row?.connectTimeoutMs, 10), requestTimeoutSec: sec(row?.requestTimeoutMs, 120),
    idleTimeoutSec: sec(row?.idleTimeoutMs, 60), maxRetries: num(row?.maxRetries, 2), maxConcurrent: num(row?.maxConcurrent, 0),
    status: row?.status === "enabled" ? "enabled" : "disabled", version: num(row?.version), hasApiKey: row?.hasApiKey === true
  };
}

export function settingsValidateProvider(d: SettingsProviderDraft, providers: Row[], editing: boolean): string {
  const code = d.code.trim();
  if (!SETTINGS_CODE_RE.test(code) || code.length > 32) return "厂商编码只能用小写字母、数字、下划线与连字符，以字母或数字开头，最长 32 位";
  if (!editing && providers.some(p => p.code === code)) return `编码「${code}」已存在`;
  if (!d.name.trim()) return "请填写厂商名称";
  if (chars(d.name.trim()) > 64) return "厂商名称不能超过 64 个字";
  try {
    const u = new URL(d.baseUrl.trim());
    if (u.protocol !== "http:" && u.protocol !== "https:") throw new Error("scheme");
  } catch {
    return "地址必须是 http:// 或 https:// 开头的完整地址";
  }
  const volc = d.authType === "volc_v4";
  if (d.keyAction === "replace") {
    if (volc) {
      const ak = d.keyAk.trim();
      const sk = d.keySk.trim();
      if ((!ak || !sk) && (ak || sk || editing || d.status === "enabled")) return "请同时填写 AccessKey 与 SecretKey（不想换就选「保留」）";
      if (ak.includes(":")) return "AccessKey 里不应有冒号，请检查是不是把两段粘到了一起";
      if (settingsLooksMasked(ak) || settingsLooksMasked(sk)) return "填的值看起来是掩码，请粘贴真实的 AccessKey / SecretKey";
    } else {
      if (!d.keyValue.trim() && (editing || d.status === "enabled")) return "请填写新的 API Key（不想换就选「保留」）";
      if (settingsLooksMasked(d.keyValue)) return "填的值看起来是掩码，请粘贴真实的 API Key";
    }
  }
  if (volc && (!d.volcRegion.trim() || !d.volcService.trim())) return "火山 V4 签名要填地域与服务";
  const replaced = volc ? Boolean(d.keyAk.trim() && d.keySk.trim()) : Boolean(d.keyValue.trim());
  const willHaveKey = d.keyAction === "replace" ? replaced : d.keyAction === "keep" && d.hasApiKey;
  if (d.status === "enabled" && d.authType !== "none" && !willHaveKey) return "启用前必须配置 API Key";
  const names = d.headers.map(h => h.name.trim()).filter(Boolean);
  if (d.headers.some(h => !h.name.trim() && h.value.trim())) return "自定义请求头的名称不能为空";
  if (new Set(names.map(n => n.toLowerCase())).size !== names.length) return "自定义请求头有重名（不区分大小写）";
  if (names.some(isVolcKey)) return `${SETTINGS_VOLC_REGION_HEADER} / ${SETTINGS_VOLC_SERVICE_HEADER} 在上面的地域与服务里填，别写进自定义请求头`;
  const [c, r, i] = [d.connectTimeoutSec, d.requestTimeoutSec, d.idleTimeoutSec];
  if (!c || !r || !i || c < 1 || r < 1 || i < 1) return "连接 / 请求 / 空闲超时都必须大于 0";
  if (i > r) return "空闲超时不应大于整体请求超时";
  if (d.maxConcurrent === null || d.maxConcurrent < 0) return "并发上限不能为负（0 表示不限）";
  if (d.maxRetries === null || d.maxRetries < 0) return "重试次数不能为负";
  return "";
}

/** 请求体。⚠️ 密钥**只在替换 / 清除时**带上。 */
export function settingsProviderPayload(d: SettingsProviderDraft) {
  const headers: Record<string, string> = {};
  for (const h of d.headers) if (h.name.trim() && !isVolcKey(h.name)) headers[h.name.trim()] = h.value;
  if (d.authType === "volc_v4") {
    headers[SETTINGS_VOLC_REGION_HEADER] = d.volcRegion.trim();
    headers[SETTINGS_VOLC_SERVICE_HEADER] = d.volcService.trim();
  }
  const body: Record<string, unknown> = {
    code: d.code.trim(), name: d.name.trim(), adapterType: d.adapterType.trim() || "configurable_http", baseUrl: d.baseUrl.trim(),
    authType: d.authType, headers, testPath: d.testPath.trim(),
    connectTimeoutMs: (d.connectTimeoutSec ?? 0) * 1000, requestTimeoutMs: (d.requestTimeoutSec ?? 0) * 1000,
    idleTimeoutMs: (d.idleTimeoutSec ?? 0) * 1000, maxRetries: d.maxRetries ?? 0, maxConcurrent: d.maxConcurrent ?? 0,
    status: d.status, version: d.version
  };
  const apiKey = settingsSecretPayload(d.keyAction, settingsKeyValue(d));
  if (apiKey) body.apiKey = apiKey;
  return body;
}

/** 实际提交的密钥值：`volc_v4` 两段拼成 `AK:SK`（服务端按第一个冒号拆）。 */
export function settingsKeyValue(d: Pick<SettingsProviderDraft, "authType" | "keyValue" | "keyAk" | "keySk">): string {
  if (d.authType === "volc_v4") return d.keyAk.trim() && d.keySk.trim() ? `${d.keyAk.trim()}:${d.keySk.trim()}` : "";
  return d.keyValue;
}

/**
 * 地址是不是局域网 / 本机。是的话要「常规 → 允许访问局域网上游」打开才调得通 ——
 * 不提示的话保存成功、连接测试却报「出网策略拒绝」，看着像地址写错。
 * 只按字面判（不解析域名）：`*.local`、localhost、私有 / 回环 / 链路本地 IPv4、IPv6 回环与 ULA。
 */
export function settingsIsPrivateAddress(baseUrl: string): boolean {
  let host = "";
  try {
    host = new URL(baseUrl.trim()).hostname.toLowerCase();
  } catch {
    return false;
  }
  host = host.replace(/^\[|\]$/g, "");
  if (host === "localhost" || host.endsWith(".localhost") || host.endsWith(".local") || host.endsWith(".lan")) return true;
  const v4 = host.match(/^(\d+)\.(\d+)\.(\d+)\.(\d+)$/);
  if (v4) {
    const [a, b] = [Number(v4[1]), Number(v4[2])];
    return a === 10 || a === 127 || a === 0 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) || (a === 169 && b === 254)
      || (a === 100 && b >= 64 && b <= 127);
  }
  return host === "::1" || /^f[cd][0-9a-f]{2}:/.test(host) || /^fe80:/.test(host);
}

// ---------------------------------------------------------------- 操作绑定与能力矩阵

export const SETTINGS_INPUT_ROLES = [
  { value: "text", label: "文本" },
  { value: "image", label: "图片" },
  { value: "first_frame", label: "首帧" },
  { value: "last_frame", label: "尾帧" },
  { value: "reference", label: "参考" },
  { value: "audio", label: "音频" },
  { value: "video", label: "视频" }
] as const;

export interface SettingsCapabilityDraft { role: string; minCount: number | null; maxCount: number | null; allowedMime: string[] }

export function settingsValidateCapability(d: SettingsCapabilityDraft, existing: Array<{ role: string }>, editing: boolean): string {
  if (!SETTINGS_INPUT_ROLES.some(r => r.value === d.role)) return "请选择输入角色";
  if (!editing && existing.some(c => c.role === d.role)) return "这个角色已经配过了：直接编辑那一条";
  const [min, max] = [d.minCount, d.maxCount];
  if (min === null || max === null || !Number.isInteger(min) || !Number.isInteger(max) || min < 0) return "数量必须是非负整数";
  if (max < min) return `最多（${max}）不能小于最少（${min}）`;
  if (max === 0) return "最少与最多同时为 0 没有意义：不接受这类输入的话删掉这一条";
  if (d.allowedMime.some(m => !/^[a-z]+\/[a-z0-9.+*-]+$/i.test(m.trim()))) return "MIME 类型的格式应为 image/png 这样的「类型/子类型」";
  return "";
}

export const settingsCapabilityPayload = (operation: string, d: SettingsCapabilityDraft) => ({
  operation, role: d.role, minCount: d.minCount ?? 0, maxCount: d.maxCount ?? 0,
  allowedMime: d.allowedMime.map(m => m.trim()).filter(Boolean)
});

// ---------------------------------------------------------------- 参数支持（model_param_support）

export interface SettingsParamSupportDraft {
  paramCode: string;
  /** 可选值，逗号或换行分隔；空 = 不限定（沿用节点通用配置的选项）。 */
  allowedText: string;
  defaultValue: string;
  /** 控件覆盖的 JSON 原文；空 = 不覆盖。 */
  overrideText: string;
}

export function settingsParamSupportDraft(row: Row | null): SettingsParamSupportDraft {
  const allowed = Array.isArray(row?.allowedValues) ? (row?.allowedValues as unknown[]).map(v => String(v)) : [];
  const override = row?.override && typeof row.override === "object" ? JSON.stringify(row.override, null, 2) : "";
  return { paramCode: str(row?.paramCode), allowedText: allowed.join(", "), defaultValue: str(row?.defaultValue), overrideText: override };
}

export function settingsParseAllowed(text: string): string[] {
  return text.split(/[,，\n]/).map(s => s.trim()).filter(Boolean);
}

/** 与服务端 `ValidateParamSupport` 同一套规则（默认值必须在可选项里 —— 读取时会把不在里面的悄悄丢掉）。 */
export function settingsValidateParamSupport(d: SettingsParamSupportDraft): string {
  if (!d.paramCode) return "请选择参数";
  const allowed = settingsParseAllowed(d.allowedText);
  let override: Record<string, unknown> | null = null;
  if (d.overrideText.trim()) {
    try {
      const v = JSON.parse(d.overrideText);
      if (!v || typeof v !== "object" || Array.isArray(v)) return "控件覆盖必须是 JSON 对象";
      override = v as Record<string, unknown>;
    } catch {
      return "控件覆盖不是合法的 JSON";
    }
  }
  if (!allowed.length && !override && !d.defaultValue.trim()) return "可选值、默认值、控件覆盖至少填一项（全空的话直接删除这一行）";
  const def = d.defaultValue.trim();
  if (def) {
    const options = override && Array.isArray(override.options)
      ? (override.options as unknown[]).map(o => (o && typeof o === "object" ? String((o as Row).value ?? (o as Row).code ?? "") : String(o)))
      : allowed;
    if (options.length && !options.includes(def)) return `默认值「${def}」不在可选项里`;
  }
  return "";
}

export function settingsParamSupportPayload(d: SettingsParamSupportDraft) {
  const allowed = settingsParseAllowed(d.allowedText);
  return {
    paramCode: d.paramCode,
    allowedValues: allowed.length ? allowed : null,
    defaultValue: d.defaultValue.trim(),
    override: d.overrideText.trim() ? JSON.parse(d.overrideText) : null
  };
}

// ---------------------------------------------------------------- 模板模式

export interface SettingsTemplateModel { code: string; name: string; domain: string; installed: boolean; status: string }
export interface SettingsTemplate {
  code: string;
  name: string;
  baseUrl: string;
  authType: string;
  headerName?: string;
  installed: boolean;
  status: string;
  hasApiKey: boolean;
  isReady: boolean;
  version: number;
  models: SettingsTemplateModel[];
}

/** 卡片的状态：没装上 / 未填 Key / 已填 Key 未启用 / 部分启用 / 已启用。 */
export function settingsTemplateState(t: SettingsTemplate): { label: string; type: "info" | "warning" | "success" | "danger" } {
  if (!t.installed) return { label: "未安装（重启应用后写入）", type: "danger" };
  const on = t.models.filter(m => m.status === "enabled").length;
  if (t.status === "enabled" && on > 0) {
    return on === t.models.length ? { label: "已启用", type: "success" } : { label: `已启用 ${on}/${t.models.length}`, type: "success" };
  }
  if (t.hasApiKey) return { label: "已填 Key，未启用", type: "warning" };
  return { label: "未填 Key", type: "info" };
}

/** 卡片上 Key 输入框的说明：放在哪个请求头里。 */
export function settingsTemplateAuthHint(t: Pick<SettingsTemplate, "authType" | "headerName">): string {
  if (t.authType === "bearer") return "以 Authorization: Bearer 发送";
  if (t.authType === "header") return `以请求头 ${t.headerName || "X-API-Key"} 发送`;
  if (t.authType === "volc_v4") return "火山 V4 签名：AccessKey 与 SecretKey";
  return "不需要 Key";
}

export interface SettingsEnableResult { code: string; name: string; enabled: boolean; skipped?: boolean; reason?: string }

/** 启用结果的一句话总结（部分失败时把失败的点名）。 */
export function settingsEnableSummary(models: SettingsEnableResult[]): { text: string; ok: boolean } {
  const failed = models.filter(m => !m.enabled);
  const on = models.length - failed.length;
  if (!failed.length) return { text: `已启用 ${on} 个模型`, ok: true };
  const names = failed.map(m => `${m.name}：${m.reason || "未知原因"}`).join("；");
  return { text: on ? `已启用 ${on} 个，${failed.length} 个未启用（${names}）` : `没有模型被启用（${names}）`, ok: false };
}
