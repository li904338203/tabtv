/**
 * 智能剪辑里「要调用模型」的功能总开关（2026-10-29 用户决定：这一版只保留手动剪辑）。
 *
 * 关着时隐藏：节点空态的四种剪辑方式、指令面板的要求输入框 / 剪辑方式 / 规格 / 发送；剪辑台的 AI 剪辑条
 * （接着提要求、发送、按要求剪一版）、规划结果、「按新素材重新剪」；识别字幕、翻译；补镜、重新生成、去字幕、
 * 生成音效、生成音频（配乐 / 音效）、生成记录。连素材、素材条、手动剪辑、文字与字幕、导出与发布照常。
 *
 * 只在前端隐藏：后端接口与已配的模型都不动，打开开关（`NUXT_PUBLIC_SMARTCUT_AI=true`）就全部回来。
 * Agent / MCP 那侧的 AI 剪辑由后端 `SEEAITV_SMARTCUT_AI_ENABLED` 管。
 *
 * ⚠️ 这个文件会进画布页的首屏包（节点卡、BaseNode 都用它）：**别从这里 import 剪辑台的东西**（如 smartcutStudio），
 *    否则剪辑台的代码被一起带进首屏，包体积检查会红（实测 +1.6 KB）。
 */
/** 运行时配置里的取值 → 开没开（环境变量来的可能是字符串）。 */
export function smartcutAiEnabledFrom(value: unknown): boolean {
  return value === true || value === "true" || value === "1" || value === 1;
}
