<script setup lang="ts">
/**
 * 字幕轨编辑（SCT-22，SC-F-50~56）：多条字幕轨（语言、隐藏、上下顺序 —— 第一条在预览与成片里最下）、轨道统一样式、
 * 逐条改文字与起止（成片时间）、在播放头处加一条、质量提示（只提示不拦）。
 *
 * - 跟随片段的条目显示「跟随第 N 段」；被片段裁掉的显示「已隐藏（片段被裁切）」—— 隐藏不是删除，区间恢复后会重新出现；
 * - 改了跟随条目的起止，它就不再跟随片段（`smartcutPatchSubtitle` 去掉锚点）：界面上先说清楚；
 * - 每个字段失焦 / 回车才提交（一个字段一步撤销），硬错误（起止颠倒、空内容）拦下这一条并说明。
 * 条目很多（上限 2000）时只渲染播放头附近的 300 条，列表照样能滚到。
 */
import { ElMessage } from "element-plus";
import type { components } from "@tabtv/api-client";
import { smartcutPatchTrack } from "~/utils/smartcutEdit";
import { smartcutTempId } from "~/utils/smartcutModel";
import {
  smartcutAddSubtitle,
  smartcutAddSubtitleTrack,
  smartcutMergeStyle,
  smartcutMoveTrack,
  smartcutPatchSubtitle,
  smartcutRemoveSubtitles,
  smartcutSubtitleTrackOf,
  type SmartcutSubtitlePatch
} from "~/utils/smartcutPanels";
import { smartcutLangLabel, smartcutTrackLabel } from "~/utils/smartcutStudio";
import { smartcutSubtitleIssues } from "~/utils/smartcutTimeline";
import type { SmartcutEditResult } from "~/utils/smartcutContent";

type Timeline = components["schemas"]["Timeline"];
type TimelineTrack = components["schemas"]["TimelineTrack"];

const props = defineProps<{ focusId?: string | null; editable: boolean; disabledReason: string }>();
/** AI 关着时（这一版只保留手动剪辑）不出识别字幕与翻译：两者都要调用模型。 */
const features = useSmartcutFeatures();
/** 识别字幕、翻译这条轨（SCT-36）：确认框由剪辑台打开（要盖住整个剪辑台、留在它的焦点陷阱里）。 */
const emit = defineEmits<{ transcribe: []; translate: [trackId: string] }>();
const store = useSmartcutStore();

const MAX_ROWS = 300;
const LANGS = ["zh-CN", "en", "ja", "ko"];

const tracks = computed(() => (store.timeline?.tracks ?? []).filter(t => t.kind === "subtitle"));
const trackId = ref("");
watch([() => props.focusId, tracks], () => {
  const t = store.timeline;
  const focused = props.focusId ? t?.subtitles.find(s => s.id === props.focusId) : undefined;
  if (focused && t) trackId.value = smartcutSubtitleTrackOf(t, focused);
  else if (!tracks.value.some(x => x.id === trackId.value)) {
    // 新建的轨道保存后临时 ID 换成了正式 ID：跟着换过去，别跳回第一条（SCT-22 浏览器自查时撞上）
    const mapped = store.currentId(trackId.value);
    trackId.value = tracks.value.some(x => x.id === mapped) ? mapped : tracks.value[0]?.id ?? "";
  }
}, { immediate: true });
const track = computed<TimelineTrack | null>(() => tracks.value.find(t => t.id === trackId.value) ?? null);

const items = computed(() => {
  const t = store.timeline;
  if (!t || !track.value) return [];
  return t.subtitles.filter(s => smartcutSubtitleTrackOf(t, s) === track.value!.id).sort((a, b) => a.startMs - b.startMs);
});
/** 渲染窗口：焦点条目或播放头附近的 300 条。 */
const windowed = computed(() => {
  const list = items.value;
  if (list.length <= MAX_ROWS) return { rows: list, offset: 0 };
  let center = props.focusId ? list.findIndex(s => s.id === props.focusId) : -1;
  if (center < 0) center = Math.max(0, list.findIndex(s => s.endMs > store.playheadMs));
  const offset = Math.max(0, Math.min(list.length - MAX_ROWS, center - Math.floor(MAX_ROWS / 2)));
  return { rows: list.slice(offset, offset + MAX_ROWS), offset };
});
const issues = computed(() => smartcutSubtitleIssues(items.value.map(s => ({ ...s, trackId: track.value?.id })), store.timeline?.effectiveMs ?? 0));
const clipIndex = computed(() => {
  const video = store.timeline?.tracks.find(t => t.kind === "video");
  return new Map((video?.clips ?? []).map((c, i) => [c.id, i + 1]));
});

function run(mutate: (t: Timeline) => SmartcutEditResult<Timeline> | Timeline): boolean {
  if (!props.editable) return false;
  let failure = "";
  const changed = store.commit((t) => {
    const r = mutate(t);
    if ("ok" in r && typeof r.ok === "boolean") {
      if (!r.ok) {
        failure = r.reason;
        return t;
      }
      return r.value;
    }
    return r as Timeline;
  });
  if (failure) ElMessage.warning(failure);
  return changed;
}

function patchItem(id: string, patch: SmartcutSubtitlePatch) {
  run(t => smartcutPatchSubtitle(t, store.currentId(id), patch));
}

function onTime(id: string, key: "startMs" | "endMs", event: Event) {
  const v = Number((event.target as HTMLInputElement).value);
  if (!Number.isFinite(v)) return;
  patchItem(id, { [key]: Math.round(v * 1000) });
}

function onText(id: string, event: Event) {
  patchItem(id, { text: (event.target as HTMLInputElement).value });
}

function addAtPlayhead() {
  if (!track.value) return;
  const id = smartcutTempId();
  if (run(t => smartcutAddSubtitle(t, track.value!.id, store.playheadMs, id))) store.select({ kind: "subtitle", ids: [id] });
}

function addTrack() {
  const id = smartcutTempId();
  const used = new Set(tracks.value.map(t => t.lang));
  if (run(t => smartcutAddSubtitleTrack(t, id, LANGS.find(l => !used.has(l))))) trackId.value = id;
}

function patchTrack(patch: Parameters<typeof smartcutPatchTrack>[2]) {
  if (!track.value) return;
  run(t => smartcutPatchTrack(t, store.currentId(track.value!.id), patch));
}

// ---------------------------------------------------------------- 轨道统一样式

const style = ref({ sizePct: 4.5, color: "#ffffff", strokePct: 0.3, bottomPct: 8 });
watch(track, (t) => {
  const s = (t?.style ?? {}) as Record<string, unknown>;
  const stroke = (s.stroke && typeof s.stroke === "object" ? s.stroke : {}) as Record<string, unknown>;
  style.value = {
    sizePct: typeof s.fontSizePct === "number" ? s.fontSizePct : 4.5,
    color: typeof s.color === "string" ? s.color : "#ffffff",
    strokePct: typeof stroke.widthPct === "number" ? stroke.widthPct : 0.3,
    bottomPct: typeof s.bottomPct === "number" ? s.bottomPct : 8
  };
}, { immediate: true });

function applyStyle() {
  if (!track.value) return;
  const merged = smartcutMergeStyle(track.value.style ?? {}, {
    fontSizePct: Number(style.value.sizePct),
    color: style.value.color,
    bottomPct: Number(style.value.bottomPct),
    stroke: { widthPct: Number(style.value.strokePct) }
  });
  patchTrack({ style: merged });
}

watch(() => props.focusId, async (id) => {
  if (!id) return;
  await nextTick();
  document.querySelector(`[data-subtitle-row="${CSS.escape(id)}"]`)?.scrollIntoView({ block: "nearest" });
});
</script>

<template>
  <section class="sc-panel" aria-label="字幕" data-testid="smartcut-subtitle-editor">
    <div class="sc-section">
      <h3>字幕轨 <button type="button" class="sc-btn" :disabled="!editable || tracks.length >= 4" :title="editable ? '新建一条字幕轨（最多 4 条）' : disabledReason" aria-label="新建字幕轨" @click="addTrack">＋ 字幕轨</button></h3>
      <div v-if="tracks.length" class="sc-seg" role="tablist" aria-label="字幕轨">
        <button
          v-for="(t, i) in tracks"
          :key="t.id"
          type="button"
          role="tab"
          class="sc-btn"
          :class="{ active: t.id === trackId }"
          :aria-selected="t.id === trackId"
          @click="trackId = t.id"
        >
          {{ smartcutTrackLabel(t, i + 1) }}{{ t.isHidden ? "（已隐藏）" : "" }}
        </button>
      </div>
      <p v-else class="sc-note">还没有字幕轨。</p>
      <div v-if="features.ai" class="sc-seg">
        <button type="button" class="sc-btn" :disabled="!editable" :title="editable ? '识别片段里的语音，生成跟随片段的字幕（按模型计费）' : disabledReason" data-testid="smartcut-subtitle-transcribe" @click="emit('transcribe')">识别字幕…</button>
        <button type="button" class="sc-btn" :disabled="!editable || !items.length" :title="!editable ? disabledReason : items.length ? '把这条字幕轨译成中 / 英 / 日 / 韩（免费）' : '这条字幕轨上还没有字幕'" data-testid="smartcut-subtitle-translate" @click="emit('translate', track?.id ?? '')">翻译…</button>
      </div>
      <p v-if="!editable" class="sc-note">{{ disabledReason }}</p>
    </div>

    <template v-if="track">
      <div class="sc-section">
        <h3>这条轨</h3>
        <div class="sc-field">
          <label for="sc-st-lang">语言</label>
          <select id="sc-st-lang" class="sc-select" :value="track.lang ?? ''" :disabled="!editable" @change="patchTrack({ lang: ($event.target as HTMLSelectElement).value || null })">
            <option value="">未指定</option>
            <option v-for="l in LANGS" :key="l" :value="l">{{ smartcutLangLabel(l) }}</option>
            <option v-if="track.lang && !LANGS.includes(track.lang)" :value="track.lang">{{ track.lang }}</option>
          </select>
        </div>
        <div class="sc-seg">
          <button type="button" class="sc-btn" :class="{ active: track.isHidden }" :aria-pressed="Boolean(track.isHidden)" :disabled="!editable" :title="editable ? '隐藏后不出现在预览与成片里' : disabledReason" @click="patchTrack({ isHidden: !track.isHidden })">{{ track.isHidden ? "显示" : "隐藏" }}</button>
          <button type="button" class="sc-btn" :disabled="!editable" title="往下挪一层（第一条字幕轨在画面最下）" aria-label="字幕轨上移" @click="run(t => smartcutMoveTrack(t, store.currentId(track!.id), -1))">↑</button>
          <button type="button" class="sc-btn" :disabled="!editable" title="往上挪一层" aria-label="字幕轨下移" @click="run(t => smartcutMoveTrack(t, store.currentId(track!.id), 1))">↓</button>
        </div>
        <div class="sc-field"><label for="sc-st-size">字号（画面高度 %）</label><input id="sc-st-size" v-model.number="style.sizePct" class="sc-input" type="number" min="2" max="20" step="0.5" :disabled="!editable"></div>
        <div class="sc-field"><label for="sc-st-color">颜色</label><input id="sc-st-color" v-model="style.color" class="sc-input" type="color" :disabled="!editable"></div>
        <div class="sc-field"><label for="sc-st-stroke">描边（%）</label><input id="sc-st-stroke" v-model.number="style.strokePct" class="sc-input" type="number" min="0" max="5" step="0.1" :disabled="!editable"></div>
        <div class="sc-field"><label for="sc-st-bottom">离底边（%）</label><input id="sc-st-bottom" v-model.number="style.bottomPct" class="sc-input" type="number" min="0" max="50" step="1" :disabled="!editable"></div>
        <button type="button" class="sc-btn" :disabled="!editable" :title="editable ? '对这条轨的全部字幕生效' : disabledReason" data-testid="smartcut-subtitle-style-apply" @click="applyStyle">应用样式</button>
      </div>

      <div class="sc-section">
        <h3>字幕（{{ items.length }} 条） <button type="button" class="sc-btn" :disabled="!editable" :title="editable ? '在播放头处加一条（2 秒）' : disabledReason" data-testid="smartcut-subtitle-add" @click="addAtPlayhead">在播放头处加一条</button></h3>
        <ul v-if="issues.length" class="sc-issues" role="note" aria-label="字幕提示">
          <li v-for="issue in issues.slice(0, 8)" :key="issue.message">{{ issue.message }}</li>
          <li v-if="issues.length > 8">还有 {{ issues.length - 8 }} 条提示</li>
        </ul>
        <p v-if="items.length > MAX_ROWS" class="sc-note">共 {{ items.length }} 条，这里显示第 {{ windowed.offset + 1 }}~{{ windowed.offset + windowed.rows.length }} 条（跟着播放头与选中的那条）。</p>
        <ul class="sc-sub-list">
          <li
            v-for="(sub, i) in windowed.rows"
            :key="sub.id"
            :class="{ focused: sub.id === focusId, hidden: sub.hidden }"
            :data-subtitle-row="sub.id"
          >
            <div class="sc-sub-head">
              <span>{{ windowed.offset + i + 1 }}</span>
              <span v-if="sub.anchor" class="sc-tag" :title="'改起止之后它就不再跟随片段'">跟随第 {{ clipIndex.get(sub.anchor.clipId) ?? "?" }} 段</span>
              <span v-if="sub.hidden" class="sc-tag">已隐藏（片段被裁切）</span>
              <button type="button" class="sc-btn icon" :disabled="!editable" :title="editable ? '删除这条字幕' : disabledReason" aria-label="删除这条字幕" @click="run(t => smartcutRemoveSubtitles(t, [store.currentId(sub.id)]))">×</button>
            </div>
            <div class="sc-sub-times">
              <input class="sc-input" type="number" min="0" step="0.01" :value="sub.startMs / 1000" aria-label="开始（秒）" :disabled="!editable" @change="onTime(sub.id, 'startMs', $event)">
              <span>→</span>
              <input class="sc-input" type="number" min="0" step="0.01" :value="sub.endMs / 1000" aria-label="结束（秒）" :disabled="!editable" @change="onTime(sub.id, 'endMs', $event)">
            </div>
            <input class="sc-input" type="text" :value="sub.text" aria-label="字幕内容" :disabled="!editable" @change="onText(sub.id, $event)">
          </li>
        </ul>
      </div>
    </template>
  </section>
</template>

<style scoped>
.sc-issues { margin: 0; padding-left: 16px; color: var(--portal-gold); font-size: 12px; line-height: 1.6; }
.sc-sub-list { display: grid; gap: 6px; margin: 0; padding: 0; list-style: none; }
.sc-sub-list li { display: grid; gap: 4px; padding: 6px; border: 1px solid var(--portal-line); border-radius: 6px; }
.sc-sub-list li.focused { border-color: var(--portal-cyan); }
.sc-sub-list li.hidden { opacity: 0.6; border-style: dashed; }
.sc-sub-head { display: flex; align-items: center; gap: 6px; color: var(--portal-muted); }
.sc-sub-head .sc-btn { margin-left: auto; }
.sc-sub-times { display: grid; grid-template-columns: 1fr auto 1fr; align-items: center; gap: 4px; }
</style>
