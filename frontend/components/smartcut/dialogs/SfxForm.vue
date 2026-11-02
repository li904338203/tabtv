<script setup lang="ts">
/**
 * 按画面生成音效（SCT-40，SC-F-65、SC-F-133）：只把这一段取用的那几秒发给上游；模型只收一定长度的片段（可灵 3~20 秒），
 * 不在范围里时说清并不受理。完成后放进音效轨、跟随这一段（移动、裁切、变速时跟着走）。
 */
import type { SmartcutStoreGeneration } from "~/stores/smartcut";
import { useSmartcutConfig } from "~/composables/useSmartcutConfig";
import { smartcutSfxRangeReason } from "~/utils/smartcutClipGen";
import QuoteConfirmDialog from "./QuoteConfirmDialog.vue";

const props = defineProps<{ clipId: string; sourceMs: number }>();
const emit = defineEmits<{ close: []; done: [generation: SmartcutStoreGeneration] }>();
const store = useSmartcutStore();
const smartcutConfig = useSmartcutConfig();

const models = computed(() => smartcutConfig.config.value?.models?.video2audio ?? store.config?.models?.video2audio ?? []);
const modelCode = ref(models.value[0]?.code ?? "");
watch(models, (list) => { if (!modelCode.value && list[0]) modelCode.value = list[0].code; });
const prompt = ref("");
const model = computed(() => models.value.find(m => m.code === modelCode.value));
const formReason = computed(() => {
  if (!models.value.length) return "没有可用的音效模型，请联系管理员配置";
  if (prompt.value.trim().length > 1000) return "音效描述不能超过 1000 字";
  return smartcutSfxRangeReason(props.sourceMs, model.value);
});
const request = computed(() => (modelCode.value
  ? { tool: "sfx" as const, clipId: props.clipId, body: { modelCode: modelCode.value, ...(prompt.value.trim() ? { prompt: prompt.value.trim() } : {}) } }
  : null));
</script>

<template>
  <QuoteConfirmDialog :request="request" :model-name="model?.name ?? ''" :form-reason="formReason" @close="emit('close')" @done="g => emit('done', g)">
    <div class="sc-field">
      <label for="sc-sfx-model">模型</label>
      <select id="sc-sfx-model" v-model="modelCode" class="sc-select" :disabled="!models.length">
        <option v-if="!models.length" value="">没有可用的音效模型</option>
        <option v-for="m in models" :key="m.code" :value="m.code">
          {{ m.name }}{{ m.minSec || m.maxSec ? ` · ${m.minSec}~${m.maxSec} 秒` : "" }}
        </option>
      </select>
    </div>
    <div class="sc-field">
      <label for="sc-sfx-prompt">描述（可选）</label>
      <input id="sc-sfx-prompt" v-model="prompt" class="sc-input" maxlength="1000" placeholder="例如：雨声、脚步声、远处的雷声" data-testid="smartcut-sfx-prompt">
    </div>
    <p class="sc-note">按这一段的画面生成；放进音效轨后跟随这一段（移动、裁切、变速时一起走）。</p>
  </QuoteConfirmDialog>
</template>
