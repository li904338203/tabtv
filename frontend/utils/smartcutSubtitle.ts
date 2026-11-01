import type { components } from "@tabtv/api-client";
import { smartcutLangLabel } from "./smartcutStudio";

/**
 * 识别字幕与字幕翻译的界面文案（docs/agent/smartcut SCT-36，dev/03 §5.6）。纯函数：对话框与测试共用。
 *
 * ⚠️ 能不能用于口播精剪只看识别模型的**粒度**（服务端给的 `granularity`），不看模型名：同一家换个型号粒度就可能变。
 * ⚠️ 翻译的失败条目按服务端给的序号（原字幕轨按起点排序，从 1 起）说，不去匹配文字。
 */

type ModelChoice = components["schemas"]["SmartcutModelChoice"];
type Quote = components["schemas"]["SmartcutQuote"];
type TranscribeResult = components["schemas"]["SmartcutTranscribeResult"];
type TranslateResult = components["schemas"]["SmartcutTranslateResult"];

/** 识别的用途：做字幕、口播精剪（要字级时间、要保留口癖）、解说成片（要字级时间）。 */
export type SmartcutTranscribePurpose = "subtitles" | "talk" | "narr";

/** 识别模型的时间粒度怎么说。 */
export function smartcutGranularityLabel(granularity: string | undefined): string {
  switch (granularity) {
    case "char": return "字级时间";
    case "word": return "词级时间";
    case "segment": return "只有整句时间";
    case "text": return "只有整段文字";
    default: return "";
  }
}

/** 粒度够不够这个用途：口播与解说要字级或词级（剪点要落在字与字之间、字幕要逐字对齐）。 */
export function smartcutModelFits(granularity: string | undefined, purpose: SmartcutTranscribePurpose): boolean {
  if (purpose === "subtitles") return true;
  return granularity === "char" || granularity === "word";
}

/** 模型下拉里的一行：名字 · 粒度 · 单价（不够用途的照样列出并说明，不隐藏）。 */
export function smartcutSpeechModelLabel(model: Pick<ModelChoice, "name" | "granularity" | "unitPrice" | "unit">,
  purpose: SmartcutTranscribePurpose): string {
  const parts = [model.name];
  const g = smartcutGranularityLabel(model.granularity);
  if (g) parts.push(g);
  if (typeof model.unitPrice === "number") parts.push(model.unit === "per_second" ? `✦${model.unitPrice}/秒` : `✦${model.unitPrice}`);
  const label = parts.join(" · ");
  if (!smartcutModelFits(model.granularity, purpose)) return `${label}（不能用于${purpose === "talk" ? "口播精剪" : "解说成片"}）`;
  return label;
}

/**
 * 缺省选哪个模型：服务端建议的（42217 的报价用的那个）优先，其次是字级 / 词级的第一个 —— 做字幕也一样：
 * 只有整段时间的结果切不开长句，字幕会一整段挂在屏幕上；都没有时第一个（照样能做字幕）。
 */
export function smartcutDefaultSpeechModel(models: readonly ModelChoice[], purpose: SmartcutTranscribePurpose,
  preferred?: string): string {
  if (preferred && models.some(m => m.code === preferred)) return preferred;
  const fine = models.find(m => smartcutModelFits(m.granularity, "talk"));
  return (fine ?? models.find(m => smartcutModelFits(m.granularity, purpose)) ?? models[0])?.code ?? "";
}

/** 报价的一句话（确认框里）：要识别几份、多少秒、多少积分；已识别过的不再收费。 */
export function smartcutTranscribeQuoteText(quote: Quote | null | undefined): string {
  if (!quote) return "";
  if (quote.blockers?.length) return quote.blockers.map(b => b.message).join("；");
  const cached = (quote.skipped ?? []).filter(s => s.reason === "已有识别结果，不再收费").length;
  const others = (quote.skipped ?? []).length - cached;
  const tail = [cached ? `${cached} 份已识别过，不再收费` : "", others ? `${others} 份没有声音或正在识别，跳过` : ""]
    .filter(Boolean).join("；");
  if (!quote.items) return tail ? `没有要识别的素材（${tail}）` : "没有要识别的素材";
  const head = `识别 ${quote.items} 份素材，共 ${quote.durationSec ?? 0} 秒，✦${quote.credits}`;
  return tail ? `${head}（${tail}）` : head;
}

/** 受理之后的一句话：受理了几份、几份在等音轨、几份跳过、几份没受理成。 */
export function smartcutTranscribeResultText(res: TranscribeResult, subtitles: boolean): { text: string; failed: string[] } {
  const parts: string[] = [];
  if (res.items.length) parts.push(`已开始识别 ${res.items.length} 份`);
  if (res.waiting.length) parts.push(`${res.waiting.length} 份在准备音轨，准备好后自动识别`);
  const cached = res.skipped.filter(s => s.reason === "已有识别结果，不再收费").length;
  if (cached) parts.push(`${cached} 份已识别过${subtitles ? "，字幕已写入" : ""}`);
  const others = res.skipped.length - cached;
  if (others) parts.push(`${others} 份跳过`);
  if (!parts.length && !res.failed.length) parts.push("没有要识别的素材");
  let text = parts.join("；");
  if (subtitles && (res.items.length || res.waiting.length)) text += "。识别完成后字幕会自动写进时间线";
  const failed = res.failed.map(f => f.reason);
  if (res.subtitleError) failed.push(`字幕没有写入：${res.subtitleError}`);
  return { text, failed };
}

/** 翻译的目标语言：中英日韩里去掉原轨自己的语言（翻成同一种语言没有意义）。 */
export function smartcutTranslateTargets(langs: readonly string[] | undefined, sourceLang: string | null | undefined): string[] {
  const all = langs?.length ? [...langs] : ["zh-CN", "en", "ja", "ko"];
  return all.filter(l => l !== sourceLang);
}

/** 失败的序号列成「第 3、7 条」（多于 8 条时只列前 8 个）。 */
function indexList(indexes: number[]): string {
  const head = indexes.slice(0, 8).join("、");
  return indexes.length > 8 ? `第 ${head} 等 ${indexes.length} 条` : `第 ${head} 条`;
}

/** 翻译结果的一句话（SC-AC-53）：译了几条、放在哪、哪几条没译出来（那几条不改）。 */
export function smartcutTranslateResultText(res: Pick<TranslateResult, "translated" | "failed">, targetLang: string,
  mode: "newTrack" | "replace"): { text: string; ok: boolean } {
  const lang = smartcutLangLabel(targetLang) || targetLang;
  const failed = (res.failed ?? []).map(f => f.index).sort((a, b) => a - b);
  if (!res.translated) {
    const reason = res.failed?.[0]?.reason;
    return { text: `一条都没有译出来，字幕没有改${reason ? `：${reason}` : ""}`, ok: false };
  }
  const where = mode === "newTrack" ? `，放在新的${lang}字幕轨` : `，已替换成${lang}`;
  if (!failed.length) return { text: `已译好 ${res.translated} 条${where}`, ok: true };
  return { text: `已译好 ${res.translated} 条${where}；${indexList(failed)}没有译出来，${mode === "newTrack" ? "新轨里没有这几条" : "保持原文"}`, ok: true };
}
