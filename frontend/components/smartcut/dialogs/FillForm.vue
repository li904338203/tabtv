<script setup lang="ts">
/**
 * 补镜（SCT-40，SC-F-131）：在选定的位置插入一段新生成的视频；按所选模型定价。可以用前一段的末帧作参考图（图生视频）。
 * 完成后插在发起时记下的位置；那个位置已经不在时进生成记录，可以手动拖进时间线。
 */
import type { SmartcutStoreGeneration } from "~/stores/smartcut";
import { useSmartcutConfig } from "~/composables/useSmartcutConfig";
import QuoteConfirmDialog from "./QuoteConfirmDialog.vue";

const props = defineProps<{ afterClipId: string; positionText: string }>();
const emit = defineEmits<{ close: []; done: [generation: SmartcutStoreGeneration] }>();
const store = useSmartcutStore();
const smartcutConfig = useSmartcutConfig();

const models = computed(() => smartcutConfig.config.value?.models?.fill ?? store.config?.models?.fill ?? []);
const modelCode = ref(models.value[0]?.code ?? "");
watch(models, (list) => { if (!modelCode.value && list[0]) modelCode.value = list[0].code; });
const prompt = ref("");
const durationSec = ref(5);
// 用前一段的末帧作参考（图生视频）：插在最前面时没有前一段
const useRef = ref(Boolean(props.afterClipId));
const model = computed(() => models.value.find(m => m.code === modelCode.value));
const formReason = computed(() => {
  if (!models.value.length) return "没有可用的视频模型，请联系管理员配置";
  const d = Number(durationSec.value);
  if (!Number.isInteger(d) || d < 1 || d > 60) return "时长只能是 1~60 秒";
  const lo = model.value?.minSec ?? 0;
  const hi = model.value?.maxSec ?? 0;
  if ((lo && d < lo) || (hi && d > hi)) return `「${model.value?.name}」单次只能生成 ${lo}~${hi} 秒`;
  return "";
});
const request = computed(() => {
  if (!modelCode.value || !prompt.value.trim()) return null;
  return {
    tool: "fill" as const,
    ...(props.afterClipId ? { afterClipId: props.afterClipId } : {}),
    ...(useRef.value && props.afterClipId ? { refFromClipId: props.afterClipId } : {}),
    body: {
      prompt: prompt.value.trim(), modelCode: modelCode.value, durationSec: Number(durationSec.value),
      operation: useRef.value && props.afterClipId ? "img2video" : "text2video"
    }
  };
});
</script>

<template>
  <QuoteConfirmDialog :request="request" :model-name="model?.name ?? ''" :form-reason="formReason" @close="emit('close')" @done="g => emit('done', g)">
    <p class="sc-note" data-testid="smartcut-fill-position">{{ positionText }}</p>
    <div class="sc-field">
      <label for="sc-fill-prompt">要补一段什么</label>
      <textarea id="sc-fill-prompt" v-model="prompt" class="sc-textarea" rows="4" maxlength="4000" placeholder="例如：镜头从窗外推到桌上的咖啡杯" data-testid="smartcut-fill-prompt" />
    </div>
    <div class="sc-field">
      <label for="sc-fill-model">模型</label>
      <select id="sc-fill-model" v-model="modelCode" class="sc-select" :disabled="!models.length">
        <option v-if="!models.length" value="">没有可用的视频模型</option>
        <option v-for="m in models" :key="m.code" :value="m.code">{{ m.name }}</option>
      </select>
    </div>
    <div class="sc-field">
      <label for="sc-fill-duration">时长（秒）</label>
      <input id="sc-fill-duration" v-model.number="durationSec" class="sc-input" type="number" min="1" max="60" step="1">
    </div>
    <label class="sc-radio" :title="afterClipId ? '' : '插在最前面时没有前一段'">
      <input v-model="useRef" type="checkbox" :disabled="!afterClipId" data-testid="smartcut-fill-ref">用前一段的末帧作参考（图生视频）
    </label>
  </QuoteConfirmDialog>
</template>
