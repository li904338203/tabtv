/**
 * 写作类目目录。
 *
 * <p>抽出来是因为新建向导（/writing/new）和工作区（/writing/[projectId]）都要用它：
 * 向导用来选，工作区用来把 settings.category 显示成中文。
 *
 * <p>⚠️ 这份清单必须与后端 `WritingProjectProfileFactory.presets()` 的 key 逐一对应 ——
 * code 对不上时后端会静默回落到「番茄 · 玄幻脑洞」，用户选了悬疑却拿到玄幻的体裁约束。
 */
export const CATEGORY_SOURCES = [
  { code: "fanqie", label: "番茄" },
  { code: "qidian", label: "起点" }
] as const;

export type CategorySource = (typeof CATEGORY_SOURCES)[number]["code"];

export const NOVEL_CATEGORIES: Record<CategorySource, { code: string; name: string; hint: string }[]> = {
  fanqie: [
    { code: "xuanhuan", name: "玄幻脑洞", hint: "高频爽点、金手指升级" },
    { code: "dushi", name: "都市爽文", hint: "现实压力下的逆袭" },
    { code: "xianxia", name: "东方仙侠", hint: "修行体系与命运逆转" },
    { code: "kehuan", name: "科幻末世", hint: "生存危机与秩序重建" },
    { code: "xuanyi", name: "悬疑灵异", hint: "谜团、反转与真相" },
    { code: "lishi", name: "历史古代", hint: "时代压力与身份博弈" },
    { code: "youxi", name: "游戏体育", hint: "规则竞技与数值成长" },
    { code: "nvpin", name: "女频现言", hint: "情绪张力与自我成长" }
  ],
  qidian: [
    { code: "xuanhuan", name: "玄幻", hint: "宏大体系与长期成长" },
    { code: "qihuan", name: "奇幻", hint: "异世界规则与冒险" },
    { code: "xianxia", name: "仙侠", hint: "大道追求与长线因果" },
    { code: "dushi", name: "都市", hint: "现实质感与人生翻盘" },
    { code: "lishi", name: "历史", hint: "时代风云与策略经营" },
    { code: "kehuan", name: "科幻", hint: "技术假设与文明尺度" },
    { code: "youxi", name: "游戏", hint: "规则系统与竞技反馈" },
    { code: "xuanyi", name: "悬疑", hint: "谜题结构与心理压力" },
    { code: "lightnovel", name: "轻小说", hint: "鲜明角色与轻快互动" }
  ]
};

/** 向导第二步要生成的六份设定。顺序与后端 `WritingSetupPrompts.SETTING_SPECS` 一致。 */
export const SETTING_SPECS = [
  { code: "world_rule", name: "核心设定", hint: "世界规则、力量体系与代价" },
  { code: "character", name: "主要人物", hint: "主角与关键配角的欲望和短板" },
  { code: "plot_line", name: "主线推进", hint: "开局钩子到阶段性结局" },
  { code: "foreshadowing", name: "钩子与伏笔", hint: "埋设点与回收窗口" },
  { code: "style_rule", name: "文风与节奏", hint: "人称、句段节奏、要避免的写法" },
  { code: "location", name: "关键场景", hint: "反复出现的地点与它带来的压力" }
] as const;

export function sourceLabel(source?: string) {
  return CATEGORY_SOURCES.find(item => item.code === source)?.label ?? "番茄";
}

export function categoryName(source?: string, code?: string) {
  const list = NOVEL_CATEGORIES[(source as CategorySource) ?? "fanqie"] ?? NOVEL_CATEGORIES.fanqie;
  return list.find(item => item.code === code)?.name ?? "";
}

/** 向导的五步。step 与后端 writing_project.setup_step 对齐，多出的 body 是「进工作区写正文」。 */
export const SETUP_STEPS = [
  { code: "category", title: "题材与故事", hint: "选类目，写清楚要讲什么" },
  { code: "settings", title: "生成设定", hint: "世界、人物、主线、伏笔、文风、场景" },
  { code: "outline", title: "生成大纲", hint: "全书起承转合" },
  { code: "catalog", title: "生成目录", hint: "章节与每章要写什么" },
  { code: "body", title: "开始写正文", hint: "进入编辑器逐章创作" }
] as const;

/**
 * 后端的 setup_step 映射到向导的第几步（0 基）。
 *
 * <p>后端只记「下一步要做什么」，没有 category 这一档 —— 项目一旦落库，第一步必然已完成。
 */
export function stepIndexOf(setupStep?: string) {
  switch (setupStep) {
    case "settings": return 1;
    case "outline": return 2;
    case "catalog": return 3;
    case "completed": return 4;
    default: return 0;
  }
}
