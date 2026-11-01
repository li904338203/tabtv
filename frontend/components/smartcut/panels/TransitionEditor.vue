<script setup lang="ts">
/**
 * 转场设置（SCT-22，SC-F-44）：片段设置页与时间线接缝共用。类型与时长；上限按相邻两段较短者的一半（最长 3 秒），
 * 第一段的转入是「从黑场淡入」。点「应用」才提交（一步撤销）。
 *
 * ⚠️ 接缝的转场只记在后一段上（契约 TimelineClip.transitionIn）：这里编辑的永远是「这一段的转入」。
 */
import { ElMessage } from "element-plus";
import { smartcutFindClip } from "~/utils/smartcutEdit";
import { smartcutPatchClip, smartcutTransitionLimitMs } from "~/utils/smartcutPanels";
import { smartcutTransitions } from "~/utils/smartcutTimeline";

const props = defineProps<{ clipId: string; editable: boolean; disabledReason: string }>();
const store = useSmartcutStore();

const ref_ = computed(() => (store.timeline ? smartcutFindClip(store.timeline, props.clipId) : null));
const first = computed(() => ref_.value?.index === 0);
const limit = computed(() => (store.timeline ? smartcutTransitionLimitMs(store.timeline, props.clipId) : 0));
const kind = ref("none");
const durationSec = ref(0.5);

watch(() => ref_.value?.clip.transitionIn, (t) => {
  kind.value = t && t.kind !== "none" && t.durationMs > 0 ? t.kind : "none";
  durationSec.value = t && t.durationMs > 0 ? t.durationMs / 1000 : 0.5;
}, { immediate: true });

const limitText = computed(() => (limit.value >= 100 ? `最长 ${Math.round(limit.value / 100) / 10} 秒` : "片段太短，放不下转场"));

function apply() {
  if (!props.editable) return;
  let failure = "";
  const transition = kind.value === "none" ? null : { kind: kind.value as "fade", durationMs: Math.round(Number(durationSec.value) * 1000) };
  store.commit((t) => {
    const r = smartcutPatchClip(t, store.currentId(props.clipId), { transitionIn: transition });
    if (!r.ok) {
      failure = r.reason;
      return t;
    }
    return r.value;
  });
  if (failure) ElMessage.warning(failure);
}
</script>

<template>
  <div class="sc-section" data-testid="smartcut-transition-editor">
    <h3>{{ first ? "开头（从黑场淡入）" : "与上一段的转场" }}</h3>
    <div class="sc-field">
      <label :for="`sc-tr-kind-${clipId}`">类型</label>
      <select :id="`sc-tr-kind-${clipId}`" v-model="kind" class="sc-select" :disabled="!editable" :title="editable ? '' : disabledReason">
        <option v-for="t in smartcutTransitions" :key="t.kind" :value="t.kind">{{ first && t.kind !== "none" ? `${t.label}（按淡入处理）` : t.label }}</option>
      </select>
    </div>
    <div v-if="kind !== 'none'" class="sc-field">
      <label :for="`sc-tr-dur-${clipId}`">时长（秒）</label>
      <input :id="`sc-tr-dur-${clipId}`" v-model.number="durationSec" class="sc-input" type="number" min="0.1" step="0.1" :max="limit / 1000" :disabled="!editable">
    </div>
    <p class="sc-note">{{ limitText }}；预览里一律按淡入淡出近似显示。</p>
    <button type="button" class="sc-btn primary" :disabled="!editable" :title="editable ? '应用转场' : disabledReason" @click="apply">应用转场</button>
  </div>
</template>
