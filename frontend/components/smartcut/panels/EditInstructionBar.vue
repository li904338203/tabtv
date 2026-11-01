<script setup lang="ts">
/**
 * 剪辑台底部的 AI 输入条（SCT-31）：
 * - 追加指令（SC-F-110）：在已剪好的时间线上接着提要求（「第三段换短一点」「整体快一些」），AI 只改被提到的、
 *   不碰你改过的片段；结果卡说明改了哪几处；
 * - 「按要求剪一版」：用节点上的要求、模式与规格重新剪（与指令面板的「发送」同一个请求）。
 * 两者都免费、共用每小时的次数；不能用时置灰并说明原因（SC-UX-05）。
 */
import { Scissors, Send, Sparkles } from "@lucide/vue";

const props = defineProps<{
  /** 追加指令不能发的原因；空 = 能发。 */
  editDisabledReason: string;
  /** 「按要求剪一版」不能点的原因；空 = 能点。 */
  planDisabledReason: string;
  /** 发送追加指令；成功（返回非 null）后清空输入框。 */
  submit: (instruction: string) => Promise<unknown>;
}>();
const emit = defineEmits<{ plan: [] }>();

const text = ref("");
const sending = ref(false);

/** 回车发送；输入法正在选词时的回车是确认候选词，不发送（否则中文打到一半就发出去了）。 */
function onEnter(event: KeyboardEvent) {
  if (event.isComposing || event.keyCode === 229) return;
  event.preventDefault();
  void send();
}

async function send() {
  const instruction = text.value.trim();
  if (!instruction || props.editDisabledReason || sending.value) return;
  sending.value = true;
  try {
    const res = await props.submit(instruction);
    if (res) text.value = "";
  } finally {
    sending.value = false;
  }
}
</script>

<template>
  <div class="sc-aibar" data-testid="smartcut-edit-bar">
    <Sparkles :size="14" class="sc-aibar-icon" aria-hidden="true" />
    <input
      v-model="text"
      class="sc-input"
      type="text"
      maxlength="2000"
      :disabled="Boolean(editDisabledReason)"
      :placeholder="editDisabledReason || '接着提要求，例如：第三段换短一点、整体快一些、把产品镜头放到开头'"
      :title="editDisabledReason || '追加要求（免费）：AI 只改被提到的片段，你改过的不动'"
      aria-label="追加要求"
      data-testid="smartcut-edit-input"
      @keydown.enter="onEnter"
    >
    <button
      type="button"
      class="sc-btn primary"
      :disabled="Boolean(editDisabledReason) || !text.trim() || sending"
      :title="editDisabledReason || '发送追加要求（免费）'"
      data-testid="smartcut-edit-send"
      @click="send"
    ><Send :size="13" />发送</button>
    <button
      type="button"
      class="sc-btn"
      :disabled="Boolean(planDisabledReason)"
      :title="planDisabledReason || '按节点上的要求重新剪一版（免费，写入前存快照，可撤销）'"
      data-testid="smartcut-plan-again"
      @click="emit('plan')"
    ><Scissors :size="13" />按要求剪一版</button>
  </div>
</template>

<style scoped>
.sc-aibar { display: flex; align-items: center; gap: 8px; padding: 6px 12px; border-top: 1px solid var(--portal-line); background: var(--portal-panel); }
.sc-aibar-icon { flex-shrink: 0; color: var(--portal-cyan); }
.sc-aibar .sc-input { flex: 1; height: 28px; }
</style>
