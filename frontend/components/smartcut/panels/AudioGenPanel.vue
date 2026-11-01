<script setup lang="ts">
/**
 * 「生成音频」面板（SCT-40，SC-F-65、SC-F-66）：只有「配乐」「音效」两项。
 * ⚠️ 不做配音（SCK-6）：配音请在画布上用音频节点生成后连进来 —— 顶部写明，不给一个点了没用的入口。
 */
import { smartcutClipToolReason, type SmartcutClipToolContext } from "~/utils/smartcutClipGen";

const props = defineProps<{ editable: boolean; disabledReason: string; sfxContext: SmartcutClipToolContext | null }>();
const emit = defineEmits<{ close: []; music: []; sfx: [] }>();
const store = useSmartcutStore();

const musicReason = computed(() => smartcutClipToolReason("music", { editable: props.editable, disabledReason: props.disabledReason,
  models: store.config?.models?.text2music ?? undefined }));
const sfxReason = computed(() => (props.sfxContext
  ? smartcutClipToolReason("sfx", props.sfxContext)
  : props.editable ? "先在画面轨上选中一段视频" : props.disabledReason || "现在不能编辑"));
</script>

<template>
  <section class="sc-panel" aria-label="生成音频" data-testid="smartcut-audio-gen">
    <div class="sc-panel-head">
      <h3>生成音频</h3>
      <button type="button" class="sc-btn" @click="emit('close')">关闭</button>
    </div>
    <p class="sc-note" data-testid="smartcut-audio-gen-voice-note">配音请在画布上用音频节点生成后连进来：剪辑台里只生成配乐和音效。</p>
    <div class="sc-section">
      <h3>配乐</h3>
      <p class="sc-note">写一句描述，按所选模型生成一段配乐，放进配乐轨、从播放头开始。</p>
      <button type="button" class="sc-btn primary" :disabled="Boolean(musicReason)" :title="musicReason" data-testid="smartcut-audio-gen-music" @click="emit('music')">生成配乐…</button>
      <p v-if="musicReason" class="sc-note">{{ musicReason }}</p>
    </div>
    <div class="sc-section">
      <h3>音效</h3>
      <p class="sc-note">按选中的那一段画面生成音效，放进音效轨并跟随那一段。</p>
      <button type="button" class="sc-btn primary" :disabled="Boolean(sfxReason)" :title="sfxReason" data-testid="smartcut-audio-gen-sfx" @click="emit('sfx')">生成音效…</button>
      <p v-if="sfxReason" class="sc-note" data-testid="smartcut-audio-gen-sfx-reason">{{ sfxReason }}</p>
    </div>
  </section>
</template>

<style scoped>
.sc-panel-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.sc-panel-head h3 { margin: 0; font-size: 13px; }
</style>
