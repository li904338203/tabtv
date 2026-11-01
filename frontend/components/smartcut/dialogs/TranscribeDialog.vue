<script setup lang="ts">
/**
 * 语音识别的确认框（docs/agent/smartcut SCT-36，dev/03 §5.6，SC-F-120~124）：选范围、模型、是否保留口癖、语言、
 * 是否生成字幕 → 服务端报价 → 确认受理。
 *
 * - 识别**按模型收费**（SCK-4）：价格只显示服务端的报价（dryRun，逐份素材向上取整到秒），确认框写明花费、发给哪家服务、
 *   失败自动退款；
 * - 一次确认一个 `Idempotency-Key`，失败后的「重试」复用同一个键（网络抖一下重发不会多扣一次）；
 * - 不够口播 / 解说用的模型（只有整句或整段时间）照样列出并说明，不隐藏；
 * - 选了「生成字幕」：已识别过的素材当场写字幕，其余识别完成后服务端写进时间线（`timeline.updated`，reason=subtitles）。
 */
import { ElMessage } from "element-plus";
import type { components } from "@tabtv/api-client";
import { SmartcutFlushError, type SmartcutTranscribeOptions } from "~/stores/smartcut";
import { useSmartcutConfig } from "~/composables/useSmartcutConfig";
import {
  smartcutDefaultSpeechModel,
  smartcutModelFits,
  smartcutSpeechModelLabel,
  smartcutTranscribeQuoteText,
  smartcutTranscribeResultText,
  type SmartcutTranscribePurpose
} from "~/utils/smartcutSubtitle";
import { smartcutLangLabel } from "~/utils/smartcutStudio";

type Quote = components["schemas"]["SmartcutQuote"];

export type SmartcutTranscribePreset = {
  purpose: SmartcutTranscribePurpose;
  modelCode?: string;
  keepFillers?: boolean;
  subtitles?: boolean;
};

const props = defineProps<{ preset: SmartcutTranscribePreset }>();
const emit = defineEmits<{ close: [] }>();
const store = useSmartcutStore();
const smartcutConfig = useSmartcutConfig();

const LANGS = ["", "zh-CN", "en", "ja", "ko"];
const models = computed(() => smartcutConfig.config.value?.models?.speech2text ?? store.config?.models?.speech2text ?? []);
const selectedClips = computed(() => (store.selection?.kind === "clip" ? store.selection.ids : []));

const scope = ref<"all" | "selected">(selectedClips.value.length ? "selected" : "all");
const modelCode = ref(smartcutDefaultSpeechModel(models.value, props.preset.purpose, props.preset.modelCode));
// 口播精剪要靠识别结果判断口癖：从口播的出路进来时默认保留，做字幕时默认顺滑掉。
const keepFillers = ref(props.preset.keepFillers ?? props.preset.purpose === "talk");
const language = ref("");
const subtitles = ref(props.preset.subtitles ?? props.preset.purpose === "subtitles");

const quote = ref<Quote | null>(null);
const quoting = ref(false);
const busy = ref(false);
const failure = ref("");
let idemKey = "";
let quoteSeq = 0;

const model = computed(() => models.value.find(m => m.code === modelCode.value));
const fits = computed(() => smartcutModelFits(model.value?.granularity, props.preset.purpose));
const options = computed<SmartcutTranscribeOptions>(() => ({
  ...(scope.value === "selected" && selectedClips.value.length ? { clipIds: [...selectedClips.value] } : {}),
  modelCode: modelCode.value, keepFillers: keepFillers.value, subtitles: subtitles.value,
  ...(language.value ? { language: language.value } : {})
}));
const blocked = computed(() => Boolean(quote.value?.blockers?.length));
const confirmLabel = computed(() => {
  const q = quote.value;
  if (!q) return "确认识别";
  if (!q.items) return subtitles.value ? "生成字幕" : "没有要识别的素材";
  return `确认识别（✦${q.credits}）`;
});
const confirmDisabled = computed(() => busy.value || quoting.value || !modelCode.value || blocked.value
  || !quote.value || (!quote.value.items && !subtitles.value));

async function refreshQuote() {
  if (!modelCode.value) return;
  const seq = ++quoteSeq;
  quoting.value = true;
  failure.value = "";
  try {
    const q = await store.quoteTranscribe(options.value);
    if (seq === quoteSeq) quote.value = q;
  } catch (cause) {
    if (seq === quoteSeq) {
      quote.value = null;
      failure.value = cause instanceof SmartcutFlushError ? cause.message : errorText(cause, "报价失败，请检查网络后重试");
    }
  } finally {
    if (seq === quoteSeq) quoting.value = false;
  }
}

// 选项一变就重新报价（同一次确认的幂等键也作废：内容不同就是另一次请求）。
watch(options, () => {
  idemKey = "";
  void refreshQuote();
}, { immediate: true, deep: true });

function errorText(cause: unknown, fallback: string): string {
  const message = (cause as { data?: { message?: unknown }; message?: unknown } | undefined);
  const text = typeof message?.data?.message === "string" ? message.data.message : typeof message?.message === "string" ? message.message : "";
  return text && text !== "请求失败" ? text : fallback;
}

async function confirm() {
  if (confirmDisabled.value) return;
  busy.value = true;
  failure.value = "";
  try {
    idemKey ||= `smartcut-asr-${globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`}`;
    const res = await store.transcribe(options.value, idemKey);
    idemKey = "";
    const { text, failed } = smartcutTranscribeResultText(res, subtitles.value);
    ElMessage[failed.length ? "warning" : "success"](failed.length ? `${text}；${failed.join("；")}` : text);
    emit("close");
  } catch (cause) {
    failure.value = cause instanceof SmartcutFlushError ? cause.message : errorText(cause, "识别没有发起，请重试");
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <div class="sc-modal-backdrop" data-testid="smartcut-transcribe-dialog" @click.self="emit('close')">
    <section class="sc-modal" role="dialog" aria-modal="true" aria-labelledby="sc-asr-title">
      <h2 id="sc-asr-title">{{ preset.purpose === "subtitles" ? "识别字幕" : "先识别语音" }}</h2>
      <div class="sc-section">
        <div class="sc-field">
          <span>范围</span>
          <div class="sc-seg" role="radiogroup" aria-label="识别范围">
            <label class="sc-radio"><input v-model="scope" type="radio" value="all">整条时间线</label>
            <label class="sc-radio" :title="selectedClips.length ? '' : '先在时间线上选中片段'">
              <input v-model="scope" type="radio" value="selected" :disabled="!selectedClips.length">选中的 {{ selectedClips.length }} 段
            </label>
          </div>
        </div>
        <div class="sc-field">
          <label for="sc-asr-model">模型</label>
          <select id="sc-asr-model" v-model="modelCode" class="sc-select" :disabled="!models.length">
            <option v-if="!models.length" value="">没有可用的识别模型</option>
            <option v-for="m in models" :key="m.code" :value="m.code">{{ smartcutSpeechModelLabel(m, preset.purpose) }}</option>
          </select>
        </div>
        <p v-if="model && !fits" class="sc-warn">这个模型只给整句或整段时间：{{ preset.purpose === "talk" ? "口播精剪剪不了" : "解说的字幕对不准" }}，换一个字级或词级的模型。</p>
        <div class="sc-field">
          <label for="sc-asr-lang">语言</label>
          <select id="sc-asr-lang" v-model="language" class="sc-select">
            <option v-for="l in LANGS" :key="l" :value="l">{{ l ? smartcutLangLabel(l) : "自动判断" }}</option>
          </select>
        </div>
        <label class="sc-radio"><input v-model="keepFillers" type="checkbox">保留口癖（嗯、啊）</label>
        <p class="sc-note">{{ keepFillers ? "口播精剪要靠识别结果判断口癖，选它；做字幕时不选更干净。" : "识别时去掉口癖：字幕更干净，但这份结果不能用于口播精剪。" }}</p>
        <label class="sc-radio"><input v-model="subtitles" type="checkbox" data-testid="smartcut-asr-subtitles">识别完成后生成字幕（跟随片段）</label>
      </div>
      <div class="sc-section" aria-live="polite">
        <p v-if="quoting" class="sc-note">报价中…</p>
        <p v-else-if="quote" :class="blocked ? 'sc-error' : 'sc-note'" data-testid="smartcut-asr-quote">{{ smartcutTranscribeQuoteText(quote) }}</p>
        <p class="sc-note">识别会发给「{{ model?.name ?? "所选模型" }}」处理，按秒计费；识别失败自动退款。同一份素材同样的设置识别过就不再收费。</p>
        <p v-if="failure" class="sc-error" role="alert">{{ failure }}</p>
      </div>
      <div class="sc-modal-actions">
        <button type="button" class="sc-btn" @click="emit('close')">取消</button>
        <button type="button" class="sc-btn primary" :disabled="confirmDisabled" data-testid="smartcut-asr-confirm" @click="confirm">
          {{ busy ? "提交中…" : confirmLabel }}
        </button>
      </div>
    </section>
  </div>
</template>
