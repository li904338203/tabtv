<script setup lang="ts">
/**
 * 片段重拍（SCT-40，SC-F-130）：沿用这一段素材那次生成的模型、操作、参数与输入，只换提示词；按原模型报价。
 * 完成后这一段换成新素材，原素材进备选（可以切回）。
 */
import type { SmartcutStoreGeneration } from "~/stores/smartcut";
import QuoteConfirmDialog from "./QuoteConfirmDialog.vue";

const props = defineProps<{ clipId: string; initialPrompt: string; modelName: string }>();
const emit = defineEmits<{ close: []; done: [generation: SmartcutStoreGeneration] }>();

const prompt = ref(props.initialPrompt);
const request = computed(() => (prompt.value.trim()
  ? { tool: "regenerate" as const, clipId: props.clipId, body: { prompt: prompt.value.trim() } }
  : null));
const formReason = computed(() => (prompt.value.trim().length > 4000 ? "提示词不能超过 4000 字" : ""));
</script>

<template>
  <QuoteConfirmDialog :request="request" :model-name="modelName" :form-reason="formReason" @close="emit('close')" @done="g => emit('done', g)">
    <div class="sc-field">
      <label for="sc-regen-prompt">提示词</label>
      <textarea id="sc-regen-prompt" v-model="prompt" class="sc-textarea" rows="5" maxlength="4000" data-testid="smartcut-regen-prompt" />
    </div>
    <p class="sc-note">沿用原来的模型与参数，只换提示词；完成后这一段换成新结果，原素材进「备选版本」，随时可以切回。</p>
  </QuoteConfirmDialog>
</template>
