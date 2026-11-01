import { smartcutAiEnabledFrom } from "~/utils/smartcutFeatures";

/** 开发态的浏览器证据用：localStorage 里写 "1" / "0" 覆盖开关（生产构建里这段不存在，只认运行时配置）。 */
export const SMARTCUT_AI_DEV_OVERRIDE_KEY = "seeaitv:dev:smartcut-ai";

/** 智能剪辑的功能开关（见 utils/smartcutFeatures）。`ai` 关着时一切要调用模型的入口都隐藏。 */
export function useSmartcutFeatures() {
  const config = useRuntimeConfig();
  let ai = smartcutAiEnabledFrom(config.public.smartcutAi);
  if (import.meta.dev && import.meta.client) {
    try {
      const override = localStorage.getItem(SMARTCUT_AI_DEV_OVERRIDE_KEY);
      if (override === "1" || override === "0") ai = override === "1";
    } catch {
      // 存储不可用时按运行时配置
    }
  }
  return { ai };
}
