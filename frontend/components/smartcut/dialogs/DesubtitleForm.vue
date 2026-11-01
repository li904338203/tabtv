<script setup lang="ts">
/**
 * 去字幕（SCT-40，SC-F-132）：只把这一段取用的那几秒发给上游、按这段时长计价；完成后换成去掉字幕的版本，原素材与原区间
 * 进备选。模式：只去字幕（缺省）/ 去掉画面上的全部文字。
 */
import type { SmartcutStoreGeneration } from "~/stores/smartcut";
import { useSmartcutConfig } from "~/composables/useSmartcutConfig";
import QuoteConfirmDialog from "./QuoteConfirmDialog.vue";

const props = defineProps<{ clipId: string; sourceMs: number }>();
const emit = defineEmits<{ close: []; done: [generation: SmartcutStoreGeneration] }>();
const store = useSmartcutStore();
const smartcutConfig = useSmartcutConfig();

const models = computed(() => smartcutConfig.config.value?.models?.video_desubtitle ?? store.config?.models?.video_desubtitle ?? []);
const modelCode = ref(models.value[0]?.code ?? "");
watch(models, (list) => { if (!modelCode.value && list[0]) modelCode.value = list[0].code; });
const mode = ref<"subtitle" | "text">("subtitle");
const model = computed(() => models.value.find(m => m.code === modelCode.value));
const request = computed(() => (modelCode.value
  ? { tool: "desubtitle" as const, clipId: props.clipId, body: { modelCode: modelCode.value, mode: mode.value } }
  : null));
const formReason = computed(() => (models.value.length ? "" : "没有可用的去字幕模型，请联系管理员配置"));
const seconds = computed(() => Math.ceil(props.sourceMs / 1000));
</script>

<template>
  <QuoteConfirmDialog :request="request" :model-name="model?.name ?? ''" :form-reason="formReason" @close="emit('close')" @done="g => emit('done', g)">
    <div class="sc-field">
      <label for="sc-erase-model">模型</label>
      <select id="sc-erase-model" v-model="modelCode" class="sc-select" :disabled="!models.length">
        <option v-if="!models.length" value="">没有可用的去字幕模型</option>
        <option v-for="m in models" :key="m.code" :value="m.code">{{ m.name }}{{ typeof m.unitPrice === "number" ? ` · ✦${m.unitPrice}/秒` : "" }}</option>
      </select>
    </div>
    <div class="sc-field">
      <span>去掉什么</span>
      <div class="sc-seg" role="radiogroup" aria-label="去字幕模式">
        <label class="sc-radio"><input v-model="mode" type="radio" value="subtitle">字幕</label>
        <label class="sc-radio"><input v-model="mode" type="radio" value="text">画面上的全部文字</label>
      </div>
    </div>
    <p class="sc-note" data-testid="smartcut-erase-range">只处理这一段用到的 {{ seconds }} 秒（不是整段素材），按这段时长计价；完成后原素材连同原区间进「备选版本」。</p>
  </QuoteConfirmDialog>
</template>
