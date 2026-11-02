<script setup lang="ts">
/**
 * 生成配乐（SCT-40，SC-F-65）：写描述、选模型（豆包音乐 / ElevenLabs），时长缺省为从播放头到成片结尾（播放头在 0 时
 * 就是成片时长，dev/01 R-20 #8）、按所选模型的单次范围截并说明；放进配乐轨，起点为发起时的播放头（与已有配乐重叠时
 * 放进新建的配乐轨）。
 * ⚠️ 剪辑台里不生成配音（SCK-6）：配音请在画布上用音频节点生成后连进来。
 */
import type { SmartcutStoreGeneration } from "~/stores/smartcut";
import { useSmartcutConfig } from "~/composables/useSmartcutConfig";
import { smartcutMusicDuration, smartcutMusicLimits } from "~/utils/smartcutClipGen";
import { smartcutSecondsText } from "~/utils/smartcutPlan";
import { smartcutFormatTime } from "~/utils/smartcutTimeline";
import QuoteConfirmDialog from "./QuoteConfirmDialog.vue";

const props = defineProps<{ atMs: number; effectiveMs: number }>();
const emit = defineEmits<{ close: []; done: [generation: SmartcutStoreGeneration] }>();
const store = useSmartcutStore();
const smartcutConfig = useSmartcutConfig();

const models = computed(() => smartcutConfig.config.value?.models?.text2music ?? store.config?.models?.text2music ?? []);
const modelCode = ref(models.value[0]?.code ?? "");
watch(models, (list) => { if (!modelCode.value && list[0]) modelCode.value = list[0].code; });
const prompt = ref("");
/** 想要的时长（秒）；空 = 从播放头到成片结尾。 */
const wantSec = ref<number | "">("");
const instrumental = ref(true);
const model = computed(() => models.value.find(m => m.code === modelCode.value));
const plan = computed(() => smartcutMusicDuration(props.effectiveMs, props.atMs, wantSec.value === "" ? null : Number(wantSec.value), model.value));
/** 不填时长时想要多长（标签里说给用户）。 */
const defaultSec = computed(() => smartcutSecondsText(smartcutMusicDuration(props.effectiveMs, props.atMs, null, null).wantMs));
const formReason = computed(() => {
  if (!models.value.length) return "没有可用的配乐模型，请联系管理员配置";
  if (prompt.value.trim().length > 2000) return "配乐描述不能超过 2000 字";
  if (wantSec.value !== "" && (!Number.isInteger(Number(wantSec.value)) || Number(wantSec.value) < smartcutMusicLimits.minSec || Number(wantSec.value) > smartcutMusicLimits.maxSec)) {
    return `时长只能是 ${smartcutMusicLimits.minSec}~${smartcutMusicLimits.maxSec} 秒`;
  }
  return "";
});
const request = computed(() => {
  if (!modelCode.value || !prompt.value.trim()) return null;
  return {
    tool: "music" as const,
    body: {
      prompt: prompt.value.trim(), modelCode: modelCode.value, atMs: Math.max(0, Math.round(props.atMs)), instrumental: instrumental.value,
      ...(wantSec.value === "" ? {} : { durationSec: Number(wantSec.value) })
    }
  };
});
</script>

<template>
  <QuoteConfirmDialog :request="request" :model-name="model?.name ?? ''" :form-reason="formReason" @close="emit('close')" @done="g => emit('done', g)">
    <div class="sc-field">
      <label for="sc-music-prompt">想要什么样的配乐</label>
      <textarea id="sc-music-prompt" v-model="prompt" class="sc-textarea" rows="3" maxlength="2000" placeholder="例如：轻快的钢琴，适合旅行 vlog" data-testid="smartcut-music-prompt" />
    </div>
    <div class="sc-field">
      <label for="sc-music-model">模型</label>
      <select id="sc-music-model" v-model="modelCode" class="sc-select" :disabled="!models.length">
        <option v-if="!models.length" value="">没有可用的配乐模型</option>
        <option v-for="m in models" :key="m.code" :value="m.code">
          {{ m.name }}{{ m.minSec || m.maxSec ? ` · ${m.minSec}~${m.maxSec} 秒` : "" }}
        </option>
      </select>
    </div>
    <div class="sc-field">
      <label for="sc-music-duration">时长（秒）</label>
      <input
        id="sc-music-duration" v-model.number="wantSec" class="sc-input" type="number" :min="smartcutMusicLimits.minSec" :max="smartcutMusicLimits.maxSec" step="1"
        :placeholder="`不填 = 从播放头到成片结尾（${defaultSec} 秒）`" data-testid="smartcut-music-duration"
      >
    </div>
    <p v-if="plan.note" class="sc-warn" data-testid="smartcut-music-duration-note">{{ plan.note }}</p>
    <label class="sc-radio"><input v-model="instrumental" type="checkbox">纯音乐（不带人声）</label>
    <p class="sc-note">放进配乐轨，起点为播放头 {{ smartcutFormatTime(atMs) }}；与已有配乐重叠时放进新建的配乐轨。</p>
  </QuoteConfirmDialog>
</template>
