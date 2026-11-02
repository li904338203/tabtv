/**
 * 控件选项的形状归一 —— **唯一一处**。
 *
 * ⚠️ 老后端的 `param_def.config.options` 是**纯字符串数组**
 *    （`["16:9","9:16","3:4"]`，见老库 `V18__canvas_control_options.sql`），
 *    新后端 seed 发的是 **`[{label,value}]`**（带中文文案与单位，
 *    如 `{"label":"5 秒","value":"5"}`）。
 *
 * ⚠️ 这个差异**不报错，而且一次错三处**：
 *    1. `{{ option }}` 把对象 stringify 成 `{ "label": "1:1", "value": "1:1" }`
 *       —— 按钮文字从 3 个字符变成 30 个，整条工具条被撑到节点之外；
 *    2. `modelValue === option` 恒为 false（字符串比对象）→ **选中态永远不亮**；
 *    3. 点一下把**整个对象**写进 `canvas_data.params.aspect_ratio`，
 *       受理时报的是「参数校验失败」—— 指向参数**值**，而真正错的是**形状**。
 *
 * ⚠️ 判定收在这一个文件里，**不许在各控件里各写一份**：
 *    本次四个吃 options 的控件里，`select` / `tag_group` 当初写了兼容，
 *    `segmented` / `grid` 没写 —— 而没写的那两个**不会有任何编译错误**，
 *    症状只是画面上多了一堆花括号。这正是「各抄一份，漏掉的那一份不报错」。
 */

export type ControlOption =
  | string
  | number
  | {
    label?: unknown;
    name?: unknown;
    value?: unknown;
    code?: unknown;
    disabled?: unknown;
  };

function isRecord(option: ControlOption): option is Exclude<ControlOption, string | number> {
  return typeof option === "object" && option !== null;
}

/**
 * 选项的**值** —— 存进 `canvas_data.params`、发给后端的就是它。
 *
 * ⚠️ 一律归一成 string：后端的枚举值都是字符串（`"5"` 而不是 `5`），
 *    而 `param_def` 里混着裸数字时 `modelValue === value` 会因类型不同而落空。
 */
export function optionValue(option: ControlOption): string {
  if (!isRecord(option)) return String(option);
  const raw = option.value ?? option.code;
  return raw === undefined || raw === null ? "" : String(raw);
}

/** 选项的**显示文案**。没有 label 就退回值本身 —— 配置可以比前端新，不能因此空着。 */
export function optionLabel(option: ControlOption): string {
  if (!isRecord(option)) return String(option);
  const raw = option.label ?? option.name;
  return raw === undefined || raw === null || raw === "" ? optionValue(option) : String(raw);
}

/** 只有对象形态才可能被禁用（字符串选项没有承载禁用状态的地方）。 */
export function optionDisabled(option: ControlOption): boolean {
  return isRecord(option) && Boolean(option.disabled);
}

/** 从控件配置里取出选项数组；不是数组一律当空，**不抛异常**（配置可以比前端新）。 */
export function controlOptions(config: Record<string, unknown> | undefined): ControlOption[] {
  const raw = config?.options;
  return Array.isArray(raw) ? (raw as ControlOption[]) : [];
}

/**
 * 参数的**当前值**归一成字符串（没有值时为空串）。
 *
 * ⚠️ 本次修复之前，点一下控件会把**整个选项对象**写进 params（上面第 3 条）。修复挡住了新写入，
 *    但已经存下的还在：库里的视频时长是 `{"label":"10 秒","value":"10"}`，直接 `String()` 出来就是
 *    「[object Object]」，尺寸按钮上原样显示，选中态也不亮。这里按选项取值（打开节点时还会写回修正，
 *    见 `utils/canvas.normalizeLegacyParams`）。
 */
export function currentValue(modelValue: unknown): string {
  if (modelValue === undefined || modelValue === null) return "";
  return optionValue(modelValue as ControlOption);
}

/**
 * 当前值是否命中某个选项 —— 比较的是**归一后的值**，不是对象引用。
 *
 * ⚠️ 直接 `modelValue === option` 在对象形态下永远 false，
 *    而那表现为"选中态不亮"，不是报错。
 */
export function optionSelected(option: ControlOption, modelValue: unknown): boolean {
  if (modelValue === undefined || modelValue === null) return false;
  return optionValue(option) === currentValue(modelValue);
}

/**
 * 参数之间的约束（`config.restrictBy`，由型号覆盖下发，见契约 `ModelParams`）：
 * `{"restrictBy":{"resolution":{"1080p":["16:9","9:16","1:1"]}}}` 写在画幅上，
 * 意思是「清晰度为 1080p 时画幅只能选这三种」（有的视频型号就是这样）。
 *
 * 返回在 `params` 这组取值下**还能选的值**；不受任何约束时返回 null（全部可选）。
 * 多条约束同时生效时取交集。
 */
export function restrictedOptionValues(
  config: Record<string, unknown> | undefined,
  params: Record<string, unknown>
): Set<string> | null {
  const rules = config?.restrictBy;
  if (!rules || typeof rules !== "object" || Array.isArray(rules)) return null;
  const sets: Set<string>[] = [];
  for (const [otherCode, table] of Object.entries(rules as Record<string, unknown>)) {
    if (!table || typeof table !== "object" || Array.isArray(table)) continue;
    const list = (table as Record<string, unknown>)[currentValue(params[otherCode])];
    if (Array.isArray(list)) sets.push(new Set(list.map(item => optionValue(item as ControlOption))));
  }
  const [first, ...rest] = sets;
  if (!first) return null;
  return new Set([...first].filter(value => rest.every(set => set.has(value))));
}

/**
 * 某一项改动之后，受约束的其他项要不要跟着改：当前值落到约束之外时改成默认值
 * （默认值也选不了时取第一个可选项）。返回要改的 `{code: value}`，不用改时是空对象。
 *
 * ⚠️ 不校正的话：先选 21:9、再把清晰度切到 1080p —— 按钮上还显示 21:9，而上游只收三种，
 *    要到提交后才报错（那时已经扣过费）。还没取过值（空）的不动：提交时自有默认值兜底。
 */
export function restrictionFixes(
  params: Record<string, unknown>,
  configs: Record<string, Record<string, unknown>>
): Record<string, string> {
  const fixes: Record<string, string> = {};
  for (const [code, config] of Object.entries(configs)) {
    const allowed = restrictedOptionValues(config, { ...params, ...fixes });
    const current = currentValue(params[code]);
    if (!allowed || !current || allowed.has(current)) continue;
    const candidates = controlOptions(config).map(optionValue).filter(value => allowed.has(value));
    const preferred = currentValue(config.default);
    const next = candidates.includes(preferred) ? preferred : candidates[0];
    if (next !== undefined && next !== current) fixes[code] = next;
  }
  return fixes;
}

/**
 * 选项能不能画成滑块：都是整数、等差、且不少于 `minCount` 个（默认 7）。能的话返回范围，否则 null。
 *
 * 时长这类连续整数（Seedance 2.5 是 4~30 秒、27 档）铺成按钮要好几排，老系统这里本来就是滑块。
 * ⚠️ 只看**选项本身**，不另配 min/max：切模型时的校准（reconcileParams）认的是 options，
 *    范围另写一份的话，滑块拖得到的值与校准认的值迟早对不上。
 */
export function optionsSliderRange(
  options: ControlOption[],
  minCount = 7
): { min: number; max: number; step: number } | null {
  if (options.length < minCount) return null;
  const values = options.map(option => optionValue(option));
  if (values.some(value => !/^-?\d+$/.test(value))) return null;
  const numbers = values.map(Number);
  const step = numbers[1] - numbers[0];
  if (step <= 0) return null;
  for (let index = 2; index < numbers.length; index += 1) {
    if (numbers[index] - numbers[index - 1] !== step) return null;
  }
  return { min: numbers[0], max: numbers[numbers.length - 1], step };
}
