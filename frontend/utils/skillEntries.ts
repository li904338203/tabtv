/**
 * 输入框下方的技能快捷入口（创造页设计 02 §5；营销 MKT-F-20~22、MKT-T-14）。纯函数。
 *
 * 创造页与营销页共用同一条规则：运营在后台勾了 `isQuickEntry` 的技能，按 `sort` 取前 `MAX_SKILL_ENTRIES` 个。
 * 数字只在这里写一次 —— 两个页面各写一个 6，改一处忘一处的时候两边的芯片条就不一样长了。
 *
 * ⚠️ 芯片**只来自服务端的技能目录**，这里不追加任何写死的入口（「每日热点趋势」之类没有数据源的，
 *    MK-4 / MKT-AC-23）；营销页传进来的是 `GET /agent/skills?scene=marketing` 的结果。
 * ⚠️ 不改入参数组（`sort` 是原地排序，直接排会把 store 里缓存的技能列表顺序也改掉）。
 */
import type { components } from "@tabtv/api-client";

/** 技能快捷入口最多几个（设计 02 §5、MKT-F-20）。 */
export const MAX_SKILL_ENTRIES = 6;

type QuickEntrySkill = Pick<components["schemas"]["AgentSkill"], "isQuickEntry" | "sort">;

/**
 * 一次出多个变体的技能（批量生成、素材裂变，MKT-F-50~52）：价格明细据此多一行「最多 N 个变体」（MKT-UX-08）。
 *
 * 读技能目录里的 `isBatch`（运营在管理端技能编辑器勾选，迁移 00062，MKT-T-32）。
 * ⚠️ 不再按技能 code 硬匹配：此前写死批量变体与素材裂变两个技能的 code，
 *    运营换 code 或新建批量技能时这一行提示悄悄消失。目录里找不到这个 code（还没加载、已停用）按「不是批量」——
 *    只少一句说明，不影响计费（变体上限与逐项计费都在服务端）。
 */
export function isBatchSkill(
  code: string | null | undefined,
  skills: readonly Pick<components["schemas"]["AgentSkill"], "code" | "isBatch">[] | null | undefined
): boolean {
  if (!code || !skills) return false;
  return skills.find(skill => skill.code === code)?.isBatch === true;
}

/** 勾了快捷入口的技能，按 `sort` 升序（同 sort 保持目录顺序），最多 `max` 个。 */
export function quickSkillEntries<T extends QuickEntrySkill>(skills: readonly T[], max: number = MAX_SKILL_ENTRIES): T[] {
  return skills
    .map((skill, index) => ({ skill, index }))
    .filter(entry => entry.skill.isQuickEntry)
    .sort((a, b) => a.skill.sort - b.skill.sort || a.index - b.index)
    .slice(0, Math.max(0, max))
    .map(entry => entry.skill);
}
