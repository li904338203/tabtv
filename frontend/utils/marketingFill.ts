/**
 * 模板墙 →「填入输入框」的请求（MKT-T-15，营销 dev/03 §5；MK-16、MKT-AC-12）。纯函数。
 *
 * 模板墙与输入框是营销页上的两个兄弟组件，彼此不持有对方的实例：墙只**发出**一个「把这条创意填进去」的请求，
 * 输入框（MKT-T-14）**消费**它（调 `utils/marketingComposer.applyCreative` + 聚焦正文）。共享状态在
 * `composables/useMarketingFill.ts`。
 *
 * ⚠️ 填入**不发送、不建会话、不扣费**（MK-16）：请求里只有选项本身，没有任何「顺便发出去」的开关 ——
 *    留一个开关的话，迟早会有人为了「一键生成」把它打开，而那等于让用户为一条他还没看过方案的创意付钱。
 * ⚠️ 每次点击都是一个**新**请求（`seq` 递增）：同一张卡点两次（中间用户删掉了胶囊）必须再填一次，
 *    只比较选项 code 的话第二次会被当成「没变化」吞掉 —— 表现为点了没反应。
 */
import type { MarketingOption } from "./marketingOptions";

export type MarketingFillRequest = {
  /** 递增序号：区分「同一条创意被点了两次」。 */
  seq: number;
  /** 被点的创意（整份选项：`applyCreative` 要用 `defaultSkillCode` / `recommendedModels` / `inputRequirements`）。 */
  option: MarketingOption;
};

/** 由上一个请求推出下一个（无论上一个有没有被消费，序号都递增）。 */
export function nextFillRequest(prev: MarketingFillRequest | null, option: MarketingOption, lastSeq = 0): MarketingFillRequest {
  return { seq: Math.max(prev?.seq ?? 0, lastSeq) + 1, option };
}

/** 只接受创意：模板墙 = 创意库（MKD-7），Hook / 风格不该走这条路（它们没有 MK-20 联动）。 */
export function isFillableOption(option: Pick<MarketingOption, "kind"> | null | undefined): boolean {
  return option?.kind === "creative";
}
