<script setup lang="ts">
/**
 * 字幕翻译（docs/agent/smartcut SCT-36，SC-F-125 / 126）：选源字幕轨、目标语言（中英日韩之一）、新轨或替换原文。
 *
 * - 翻译算 AI 剪辑的一部分：与 AI 剪辑共用每小时次数（SCK-2），被限流时说多久后再试；
 * - 完成后说清译了几条、放在哪，没译出来的按序号说（那几条不改）；一条都没译出来时时间线不动。
 */
import { ElMessage } from "element-plus";
import { SmartcutFlushError } from "~/stores/smartcut";
import { useSmartcutConfig } from "~/composables/useSmartcutConfig";
import { smartcutSubtitleTrackOf } from "~/utils/smartcutPanels";
import { smartcutPlanErrorText } from "~/utils/smartcutPlan";
import { smartcutLangLabel, smartcutTrackLabel } from "~/utils/smartcutStudio";
import { smartcutTranslateResultText, smartcutTranslateTargets } from "~/utils/smartcutSubtitle";

const props = defineProps<{ sourceTrackId: string }>();
const emit = defineEmits<{ close: [] }>();
const store = useSmartcutStore();
const smartcutConfig = useSmartcutConfig();

const tracks = computed(() => (store.timeline?.tracks ?? []).filter(t => t.kind === "subtitle"));
const counts = computed(() => {
  const out = new Map<string, number>();
  const t = store.timeline;
  for (const s of t?.subtitles ?? []) {
    const id = smartcutSubtitleTrackOf(t!, s);
    out.set(id, (out.get(id) ?? 0) + 1);
  }
  return out;
});
const sourceId = ref(tracks.value.some(t => t.id === props.sourceTrackId) ? props.sourceTrackId : tracks.value[0]?.id ?? "");
const source = computed(() => tracks.value.find(t => t.id === sourceId.value) ?? null);
const config = computed(() => smartcutConfig.config.value ?? store.config);
const targets = computed(() => smartcutTranslateTargets(config.value?.translateLangs, source.value?.lang));
const targetLang = ref("");
watch(targets, (list) => {
  if (!list.includes(targetLang.value)) targetLang.value = list.includes("en") ? "en" : list[0] ?? "";
}, { immediate: true });
const mode = ref<"newTrack" | "replace">("newTrack");
const busy = ref(false);
const failure = ref("");

const full = computed(() => tracks.value.length >= 4);
const disabledReason = computed(() => {
  if (!source.value) return "还没有字幕轨";
  if (!(counts.value.get(source.value.id) ?? 0)) return "这条字幕轨上还没有字幕";
  if (mode.value === "newTrack" && full.value) return "字幕轨已满（最多 4 条）：选「替换原文」，或先删掉一条";
  if (store.planRunning) return "AI 剪辑进行中，请等它完成";
  return "";
});
const usage = computed(() => (config.value ? `与 AI 剪辑共用次数（本小时已用 ${config.value.usedThisHour} / ${config.value.perHour} 次）` : "与 AI 剪辑共用每小时次数"));

async function submit() {
  if (busy.value || disabledReason.value || !targetLang.value) return;
  busy.value = true;
  failure.value = "";
  try {
    const res = await store.translate({ subtitleTrackId: sourceId.value, targetLang: targetLang.value, mode: mode.value });
    const { text, ok } = smartcutTranslateResultText(res, targetLang.value, mode.value);
    void smartcutConfig.refresh();
    if (!ok) {
      failure.value = text;
      return;
    }
    ElMessage[res.failed.length ? "warning" : "success"](text);
    emit("close");
  } catch (cause) {
    if (cause instanceof SmartcutFlushError) failure.value = cause.message;
    else {
      const f = smartcutPlanErrorText(cause);
      if (f.retryAfterSeconds) smartcutConfig.markLimited(f.retryAfterSeconds);
      failure.value = f.text.replace("AI 剪辑失败", "翻译失败");
    }
    void smartcutConfig.refresh();
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <div class="sc-modal-backdrop" data-testid="smartcut-translate-dialog" @click.self="emit('close')">
    <section class="sc-modal" role="dialog" aria-modal="true" aria-labelledby="sc-tr-title">
      <h2 id="sc-tr-title">翻译字幕</h2>
      <div class="sc-section">
        <div class="sc-field">
          <label for="sc-tr-source">原字幕</label>
          <select id="sc-tr-source" v-model="sourceId" class="sc-select">
            <option v-for="(t, i) in tracks" :key="t.id" :value="t.id">{{ smartcutTrackLabel(t, i + 1) }}（{{ counts.get(t.id) ?? 0 }} 条）</option>
          </select>
        </div>
        <div class="sc-field">
          <label for="sc-tr-target">译成</label>
          <select id="sc-tr-target" v-model="targetLang" class="sc-select">
            <option v-for="l in targets" :key="l" :value="l">{{ smartcutLangLabel(l) }}</option>
          </select>
        </div>
        <div class="sc-field">
          <span>写法</span>
          <div class="sc-seg" role="radiogroup" aria-label="翻译的写法">
            <label class="sc-radio" :title="full ? '字幕轨已满（最多 4 条）' : ''"><input v-model="mode" type="radio" value="newTrack" :disabled="full">新建一条字幕轨</label>
            <label class="sc-radio"><input v-model="mode" type="radio" value="replace">替换原文</label>
          </div>
        </div>
        <p class="sc-note">译文保留原字幕的时间；跟随片段的字幕照样跟随。{{ mode === "replace" ? "替换后可以用撤销或「历史」找回原文。" : "" }}</p>
      </div>
      <div class="sc-section" aria-live="polite">
        <p class="sc-note">{{ usage }}</p>
        <p v-if="disabledReason" class="sc-warn">{{ disabledReason }}</p>
        <p v-if="failure" class="sc-error" role="alert">{{ failure }}</p>
      </div>
      <div class="sc-modal-actions">
        <button type="button" class="sc-btn" @click="emit('close')">取消</button>
        <button type="button" class="sc-btn primary" :disabled="busy || Boolean(disabledReason) || !targetLang" data-testid="smartcut-translate-confirm" @click="submit">
          {{ busy ? "翻译中…" : "翻译" }}
        </button>
      </div>
    </section>
  </div>
</template>
