<script setup lang="ts">
/**
 * 片段编辑面板（SCT-22，dev/03 §5.3）：「片段设置」与「生成信息」两页。
 *
 * - 片段设置：变速、音量、声音淡入淡出、转场（共用 TransitionEditor）、最后一段的淡出到黑场、画面变换的数值（与预览拖动联动）、
 *   备选切换（原片 / 重拍 / 去字幕）。表单是本地草稿，点「应用」才提交一次（一步撤销）—— 逐字段提交会把撤销栈刷满；
 * - 生成信息：这一段素材是怎么来的（`GET .../clips/{clipId}/origin`：提示词、模型、操作、主要参数）。接口不可用时降级成
 *   「来源：画布节点 X」，不报错。「重新生成」：改提示词、按原模型报价确认（SCT-40）。
 * - 片段工具（SCT-40）：去字幕、按画面生成音效、在这一段之后补镜；不可用时照样显示并说明原因。上一次生成失败时说清原因并给「重试」。
 *
 * ⚠️ 不拿上游节点**现在**的参数代替生成信息：节点参数可能在出图之后又改过，显示出来的就不是这一段真正用的提示词。
 */
import { ElMessage } from "element-plus";
import type { components } from "@tabtv/api-client";
import { smartcutSpeedLevels } from "~/utils/smartcutContent";
import { smartcutFindClip, smartcutSetClipSpeed, smartcutSetClipTransform } from "~/utils/smartcutEdit";
import { smartcutPlayLen } from "~/utils/smartcutLayout";
import { smartcutFadeOutLimitMs, smartcutPatchClip, smartcutSwitchAlternate } from "~/utils/smartcutPanels";
import { smartcutFormatTimecode } from "~/utils/smartcutTimecode";
import { smartcutNormalizeTransform, smartcutTransformWith } from "~/utils/smartcutTransform";
import {
  smartcutClipSourceMs,
  smartcutClipToolReason,
  smartcutGenerationKindLabels,
  smartcutLatestByClip,
  type SmartcutClipToolContext,
  type SmartcutClipToolOpen
} from "~/utils/smartcutClipGen";
import { useSmartcutConfig } from "~/composables/useSmartcutConfig";
import TransitionEditor from "./TransitionEditor.vue";

type Origin = components["schemas"]["SmartcutClipOrigin"];
type Timeline = components["schemas"]["Timeline"];

const props = defineProps<{ clipId: string; editable: boolean; disabledReason: string; focus?: "transition" | null }>();
/** AI 关着时（这一版只保留手动剪辑）不出片段级生成工具：去字幕、生成音效、补镜、重新生成、失败重试。 */
const features = useSmartcutFeatures();
const emit = defineEmits<{ "clip-tool": [open: SmartcutClipToolOpen] }>();
const store = useSmartcutStore();
const smartcutConfig = useSmartcutConfig();

const tab = ref<"settings" | "origin">("settings");
const ref_ = computed(() => (store.timeline ? smartcutFindClip(store.timeline, props.clipId) : null));
const clip = computed(() => ref_.value?.clip ?? null);
const isVideoTrack = computed(() => ref_.value?.track.kind === "video");
const input = computed(() => store.inputs.find(i => i.sourceNodeId === clip.value?.sourceNodeId));
const fps = computed(() => store.timeline?.fps ?? 30);

// ---------------------------------------------------------------- 草稿

type Draft = {
  speed: number;
  volume: number;
  fadeInSec: number;
  fadeOutSec: number;
  fadeToBlackSec: number;
  x: number;
  y: number;
  scale: number;
  rotation: number;
  crop: { left: number; top: number; right: number; bottom: number };
};
const draft = ref<Draft | null>(null);

function fromClip(): Draft | null {
  const c = clip.value;
  if (!c) return null;
  const t = smartcutNormalizeTransform(c.transform);
  return {
    speed: c.speed ?? 100,
    volume: c.volume ?? 100,
    fadeInSec: (c.fadeInMs ?? 0) / 1000,
    fadeOutSec: (c.fadeOutMs ?? 0) / 1000,
    fadeToBlackSec: c.transitionOut && c.transitionOut.durationMs > 0 ? c.transitionOut.durationMs / 1000 : 0,
    x: t?.x ?? 0,
    y: t?.y ?? 0,
    scale: t?.scale ?? 1,
    rotation: t?.rotation ?? 0,
    crop: { ...(t?.crop ?? { left: 0, top: 0, right: 0, bottom: 0 }) }
  };
}
watch(() => [props.clipId, JSON.stringify(clip.value)], () => { draft.value = fromClip(); }, { immediate: true });

const isLast = computed(() => Boolean(ref_.value && isVideoTrack.value && ref_.value.index === ref_.value.track.clips.length - 1));
const fadeToBlackLimit = computed(() => (store.timeline ? smartcutFadeOutLimitMs(store.timeline, props.clipId) : 0));
const lengthText = computed(() => (clip.value ? smartcutFormatTimecode(smartcutPlayLen(clip.value), fps.value) : ""));

/** 一次「应用」：变速 → 音量与淡入淡出（与淡出到黑场）→ 画面变换，任何一步不合法整次不改。 */
function apply() {
  const d = draft.value;
  if (!d || !props.editable) return;
  let failure = "";
  const id = store.currentId(props.clipId);
  store.commit((t) => {
    let next: Timeline = t;
    const step = (r: { ok: true; value: Timeline } | { ok: false; reason: string }) => {
      if (failure) return;
      if (!r.ok) failure = r.reason;
      else next = r.value;
    };
    step(smartcutSetClipSpeed(next, id, Math.round(Number(d.speed))));
    step(smartcutPatchClip(next, id, {
      volume: Math.round(Number(d.volume)),
      fadeInMs: Math.round(Number(d.fadeInSec) * 1000),
      fadeOutMs: Math.round(Number(d.fadeOutSec) * 1000),
      ...(isLast.value ? { transitionOut: d.fadeToBlackSec > 0 ? { kind: "fade", durationMs: Math.round(Number(d.fadeToBlackSec) * 1000) } : null } : {})
    }));
    if (isVideoTrack.value && !failure) {
      const current = smartcutFindClip(next, id)?.clip.transform;
      const transform = smartcutTransformWith(current, { x: Number(d.x), y: Number(d.y), scale: Number(d.scale), rotation: Number(d.rotation), crop: { ...d.crop } });
      next = smartcutSetClipTransform(next, id, smartcutNormalizeTransform(transform) ? transform : null);
    }
    return failure ? t : next;
  });
  if (failure) ElMessage.warning(failure);
}

function resetTransform() {
  if (!props.editable) return;
  store.commit(t => smartcutSetClipTransform(t, store.currentId(props.clipId), null));
}

function switchAlternate(index: number) {
  if (!props.editable) return;
  const landed = Object.values(store.generations).filter(g => g.clipId === props.clipId && g.landed).sort((a, b) => a.createdAt.localeCompare(b.createdAt)).at(-1);
  const kind = landed?.kind === "desubtitle" ? "desubtitle" : "regen";
  let failure = "";
  store.commit((t) => {
    const r = smartcutSwitchAlternate(t, store.currentId(props.clipId), index, kind);
    if (!r.ok) {
      failure = r.reason;
      return t;
    }
    return r.value;
  });
  if (failure) ElMessage.warning(failure);
}

const ALT_LABELS: Record<string, string> = { original: "原片", regen: "重拍", desubtitle: "去字幕" };

// ---------------------------------------------------------------- 片段工具（SCT-40）

const models = computed(() => smartcutConfig.config.value?.models ?? store.config?.models ?? {});
const mediaKind = computed<"video" | "image" | "audio">(() => {
  if (ref_.value?.track.kind === "audio") return "audio";
  return input.value?.kind === "image" ? "image" : "video";
});
function toolContext(modelsOf: readonly unknown[] | undefined): SmartcutClipToolContext {
  return {
    editable: props.editable, disabledReason: props.disabledReason, trackKind: ref_.value?.track.kind, mediaKind: mediaKind.value,
    pending: Boolean(clip.value?.pendingGenerationId), clipMs: clip.value ? smartcutClipSourceMs(clip.value) : 0,
    origin: originState.value === "ready" ? origin.value : originState.value === "unavailable" ? null : undefined,
    models: modelsOf
  };
}
const desubtitleReason = computed(() => smartcutClipToolReason("desubtitle", toolContext(models.value.video_desubtitle ?? undefined)));
const sfxReason = computed(() => smartcutClipToolReason("sfx", { ...toolContext(models.value.video2audio ?? undefined), model: models.value.video2audio?.[0] ?? null }));
const fillReason = computed(() => smartcutClipToolReason("fill", { editable: props.editable, disabledReason: props.disabledReason, models: models.value.fill ?? undefined }));
const toolTarget = computed(() => store.currentId(props.clipId));

/** 这一段最近一次片段级生成（失败时说清原因并给重试）。 */
const latest = computed(() => smartcutLatestByClip(store.generations).get(toolTarget.value) ?? null);
const latestFailed = computed(() => (latest.value?.status === "failed" && (latest.value.kind === "regen" || latest.value.kind === "desubtitle" || latest.value.kind === "sfx")
  ? latest.value
  : null));

function openTool(tool: SmartcutClipToolOpen["tool"]) {
  if (tool === "fill") {
    emit("clip-tool", { tool, afterClipId: toolTarget.value });
    return;
  }
  emit("clip-tool", { tool, clipId: toolTarget.value, ...(tool === "regenerate" ? { prompt: origin.value?.prompt ?? "", modelName: origin.value?.modelName || origin.value?.modelCode || "" } : {}) });
}

function retryFailed() {
  const g = latestFailed.value;
  if (!g) return;
  const tool = g.kind === "regen" ? "regenerate" : g.kind === "desubtitle" ? "desubtitle" : "sfx";
  const req = store.clipRequests[g.generationId];
  emit("clip-tool", {
    tool, clipId: toolTarget.value,
    ...(tool === "regenerate" ? { prompt: typeof req?.body.prompt === "string" ? req.body.prompt : origin.value?.prompt ?? "", modelName: origin.value?.modelName || "" } : {})
  });
}

// ---------------------------------------------------------------- 生成信息

const origin = ref<Origin | null>(null);
const originState = ref<"idle" | "loading" | "ready" | "unavailable">("idle");
let originSeq = 0;

async function loadOrigin() {
  const seq = ++originSeq;
  origin.value = null;
  originState.value = "loading";
  try {
    const res = await useApi()<Origin>(`/projects/${store.projectId}/smartcuts/${store.timelineId}/clips/${store.currentId(props.clipId)}/origin`);
    if (seq !== originSeq) return;
    origin.value = res;
    originState.value = "ready";
  } catch {
    if (seq !== originSeq) return;
    originState.value = "unavailable";
  }
}
watch(() => [tab.value, props.clipId], () => { if (tab.value === "origin") void loadOrigin(); });

const ORIGIN_KIND: Record<string, string> = { generation: "AI 生成", upload: "上传的素材", render: "智能剪辑的导出", director: "3D 导演台的截图或预演", unknown: "来源不明" };
const paramRows = computed(() => Object.entries(origin.value?.params ?? {}).filter(([, v]) => v !== null && v !== undefined && v !== "").map(([k, v]) => [k, typeof v === "object" ? JSON.stringify(v) : String(v)]));
const regenReason = computed(() => smartcutClipToolReason("regenerate", toolContext(undefined)));

watch(() => props.focus, (f) => { if (f === "transition") tab.value = "settings"; }, { immediate: true });
</script>

<template>
  <section v-if="clip && draft" class="sc-panel" aria-label="片段编辑" data-testid="smartcut-clip-inspector">
    <div class="sc-seg" role="tablist" aria-label="片段编辑">
      <button type="button" role="tab" class="sc-btn" :class="{ active: tab === 'settings' }" :aria-selected="tab === 'settings'" @click="tab = 'settings'">片段设置</button>
      <button type="button" role="tab" class="sc-btn" :class="{ active: tab === 'origin' }" :aria-selected="tab === 'origin'" @click="tab = 'origin'">生成信息</button>
    </div>
    <p class="sc-note">
      {{ ref_?.track.kind === "video" ? `画面第 ${(ref_?.index ?? 0) + 1} 段` : "音频片段" }} · {{ lengthText }}
      <span v-if="input"> · #{{ input.no }} {{ input.title }}</span>
    </p>
    <p v-if="clip.sourceNodeId && store.inputsFingerprint && !input" class="sc-warn">来源节点的连线已断开：片段保留，但画布上的修改不会再同步过来。</p>
    <p v-if="clip.pendingGenerationId" class="sc-warn">这一段正在生成，完成后会自动换上新结果。</p>
    <div v-else-if="latestFailed" class="sc-warn" role="note" data-testid="smartcut-clip-failed">
      上次{{ smartcutGenerationKindLabels[latestFailed.kind] }}失败：{{ latestFailed.error || "上游没有给出原因" }}（已按原批次退款）
      <button v-if="features.ai" type="button" class="sc-btn" :disabled="!editable" :title="editable ? '重新报价、确认后再发起一次' : disabledReason" data-testid="smartcut-clip-retry" @click="retryFailed">重试</button>
    </div>
    <p v-if="!editable" class="sc-note">{{ disabledReason }}</p>

    <template v-if="tab === 'settings'">
      <div v-if="isVideoTrack && features.ai" class="sc-section" data-testid="smartcut-clip-tools">
        <h3>片段工具</h3>
        <div class="sc-seg">
          <button type="button" class="sc-btn" :disabled="Boolean(desubtitleReason)" :title="desubtitleReason || '去掉这一段画面里的字幕（按用到的秒数计价）'" data-testid="smartcut-tool-desubtitle" @click="openTool('desubtitle')">去字幕</button>
          <button type="button" class="sc-btn" :disabled="Boolean(sfxReason)" :title="sfxReason || '按这一段的画面生成音效'" data-testid="smartcut-tool-sfx" @click="openTool('sfx')">生成音效</button>
          <button type="button" class="sc-btn" :disabled="Boolean(fillReason)" :title="fillReason || '在这一段之后插一段新生成的视频'" data-testid="smartcut-tool-fill" @click="openTool('fill')">在之后补镜</button>
        </div>
        <p v-if="desubtitleReason && editable" class="sc-note">去字幕：{{ desubtitleReason }}</p>
        <p v-if="sfxReason && editable" class="sc-note" data-testid="smartcut-tool-sfx-reason">生成音效：{{ sfxReason }}</p>
      </div>
      <div class="sc-section">
        <h3>速度与声音</h3>
        <div class="sc-field">
          <label for="sc-clip-speed">变速</label>
          <select id="sc-clip-speed" v-model.number="draft.speed" class="sc-select" :disabled="!editable">
            <option v-for="s in smartcutSpeedLevels" :key="s" :value="s">{{ s / 100 }}×</option>
            <option v-if="!(smartcutSpeedLevels as readonly number[]).includes(draft.speed)" :value="draft.speed">{{ draft.speed / 100 }}×</option>
          </select>
        </div>
        <div class="sc-field">
          <label for="sc-clip-volume">音量</label>
          <input id="sc-clip-volume" v-model.number="draft.volume" class="sc-input" type="number" min="0" max="200" step="5" :disabled="!editable">
        </div>
        <div class="sc-field">
          <label for="sc-clip-fadein">声音淡入（秒）</label>
          <input id="sc-clip-fadein" v-model.number="draft.fadeInSec" class="sc-input" type="number" min="0" step="0.1" :disabled="!editable">
        </div>
        <div class="sc-field">
          <label for="sc-clip-fadeout">声音淡出（秒）</label>
          <input id="sc-clip-fadeout" v-model.number="draft.fadeOutSec" class="sc-input" type="number" min="0" step="0.1" :disabled="!editable">
        </div>
        <div v-if="isLast" class="sc-field">
          <label for="sc-clip-black">淡出到黑场（秒）</label>
          <input id="sc-clip-black" v-model.number="draft.fadeToBlackSec" class="sc-input" type="number" min="0" step="0.1" :max="fadeToBlackLimit / 1000" :disabled="!editable" :title="`最长 ${fadeToBlackLimit / 1000} 秒，0 = 不淡出`">
        </div>
      </div>

      <div v-if="isVideoTrack" class="sc-section">
        <h3>画面变换 <button type="button" class="sc-btn" :disabled="!editable" title="回到等比适配、居中" aria-label="重置画面变换" @click="resetTransform">重置</button></h3>
        <div class="sc-field"><label for="sc-tf-x">水平位置 %</label><input id="sc-tf-x" v-model.number="draft.x" class="sc-input" type="number" min="-100" max="100" step="1" :disabled="!editable"></div>
        <div class="sc-field"><label for="sc-tf-y">垂直位置 %</label><input id="sc-tf-y" v-model.number="draft.y" class="sc-input" type="number" min="-100" max="100" step="1" :disabled="!editable"></div>
        <div class="sc-field"><label for="sc-tf-scale">缩放</label><input id="sc-tf-scale" v-model.number="draft.scale" class="sc-input" type="number" min="0.1" max="5" step="0.05" :disabled="!editable"></div>
        <div class="sc-field"><label for="sc-tf-rot">旋转（度）</label><input id="sc-tf-rot" v-model.number="draft.rotation" class="sc-input" type="number" min="-180" max="180" step="1" :disabled="!editable"></div>
        <div class="sc-field">
          <span>裁切 %</span>
          <div class="sc-crop">
            <input v-model.number="draft.crop.left" class="sc-input" type="number" min="0" max="45" aria-label="左边裁切百分比" title="左" :disabled="!editable">
            <input v-model.number="draft.crop.top" class="sc-input" type="number" min="0" max="45" aria-label="上边裁切百分比" title="上" :disabled="!editable">
            <input v-model.number="draft.crop.right" class="sc-input" type="number" min="0" max="45" aria-label="右边裁切百分比" title="右" :disabled="!editable">
            <input v-model.number="draft.crop.bottom" class="sc-input" type="number" min="0" max="45" aria-label="下边裁切百分比" title="下" :disabled="!editable">
          </div>
        </div>
        <p class="sc-note">也可以在预览上直接拖动、拖角缩放、拖顶上的把手旋转。</p>
      </div>
      <button type="button" class="sc-btn primary" :disabled="!editable" :title="editable ? '应用以上设置（一次撤销）' : disabledReason" data-testid="smartcut-clip-apply" @click="apply">应用</button>

      <TransitionEditor v-if="isVideoTrack" :clip-id="clipId" :editable="editable" :disabled-reason="disabledReason" />

      <div v-if="clip.alternates?.length" class="sc-section">
        <h3>备选版本</h3>
        <ul class="sc-alt-list">
          <li v-for="(alt, i) in clip.alternates" :key="`${alt.mediaId}-${i}`" data-testid="smartcut-alternate">
            <span :title="`取素材的 ${smartcutFormatTimecode(alt.sourceInMs, fps)}–${smartcutFormatTimecode(alt.sourceOutMs, fps)}`">
              {{ ALT_LABELS[alt.kind] ?? alt.kind }} · 取 {{ smartcutFormatTimecode(alt.sourceInMs, fps) }}–{{ smartcutFormatTimecode(alt.sourceOutMs, fps) }}
            </span>
            <button type="button" class="sc-btn" :disabled="!editable" :title="editable ? '换成这个版本（可撤销）' : disabledReason" @click="switchAlternate(i)">切换</button>
          </li>
        </ul>
      </div>
    </template>

    <template v-else>
      <p v-if="originState === 'loading'" class="sc-note">载入中…</p>
      <template v-else-if="originState === 'ready' && origin">
        <div class="sc-section">
          <h3>来源</h3>
          <p class="sc-note">{{ ORIGIN_KIND[origin.kind] ?? origin.kind }}{{ origin.modelName ? ` · ${origin.modelName}` : origin.modelCode ? ` · ${origin.modelCode}` : "" }}{{ origin.operationCode ? ` · ${origin.operationCode}` : "" }}</p>
          <p v-if="origin.kind === 'upload'" class="sc-note">这是上传的素材，没有生成信息。</p>
          <p v-else-if="origin.kind === 'render'" class="sc-note">来自智能剪辑的导出。</p>
        </div>
        <div v-if="origin.prompt" class="sc-section">
          <h3>提示词</h3>
          <p class="sc-origin-prompt">{{ origin.prompt }}</p>
        </div>
        <div v-if="paramRows.length" class="sc-section">
          <h3>主要参数</h3>
          <dl class="sc-origin-params">
            <template v-for="[k, v] in paramRows" :key="k"><dt>{{ k }}</dt><dd>{{ v }}</dd></template>
          </dl>
        </div>
      </template>
      <div v-else class="sc-section">
        <h3>来源</h3>
        <p class="sc-note">{{ input ? `画布节点「${input.title || `#${input.no}`}」（${input.kind === "image" ? "图片" : input.kind === "audio" ? "音频" : "视频"}）` : "剪辑台里直接放入的素材" }}</p>
        <p class="sc-note">生成信息暂时取不到，稍后再试。</p>
      </div>
      <button
        v-if="features.ai"
        type="button"
        class="sc-btn primary"
        :disabled="Boolean(regenReason)"
        :title="regenReason || '改提示词重新生成这一段（按原模型计价，原素材进备选）'"
        :aria-label="regenReason ? `重新生成：${regenReason}` : '重新生成'"
        data-testid="smartcut-tool-regenerate"
        @click="openTool('regenerate')"
      >
        重新生成…
      </button>
      <p v-if="regenReason && features.ai" class="sc-note">{{ regenReason }}</p>
    </template>
  </section>
</template>

<style scoped>
.sc-crop { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 4px; }
.sc-alt-list { display: grid; gap: 6px; margin: 0; padding: 0; list-style: none; }
.sc-alt-list li { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.sc-origin-prompt { margin: 0; padding: 8px; border-radius: 6px; color: var(--portal-text); background: var(--portal-surface-2); white-space: pre-wrap; line-height: 1.5; }
.sc-origin-params { display: grid; grid-template-columns: max-content 1fr; gap: 4px 10px; margin: 0; }
.sc-origin-params dt { color: var(--portal-muted); }
.sc-origin-params dd { margin: 0; word-break: break-all; }
</style>
