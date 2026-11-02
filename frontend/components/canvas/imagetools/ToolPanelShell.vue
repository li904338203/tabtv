<script setup lang="ts">
/**
 * 四个图片工具编辑器共用的外壳（docs/agent/imagetools IMGD-23）：标题、关闭、头部右侧插槽、重置参数、
 * 发送、禁用理由。tabtv 不计价（TT-T-14）：不显示积分，报价接口只用来做能力校验。
 *
 * ⚠️ 禁用理由只在这里排一次序（权限 > 工具不可用 > 编辑器里还缺东西 > 能力校验失败，CLAUDE.md §5.17）。
 * ⚠️ 只读成员**能打开、能看、能调**，只是发送置灰并说明权限（IMG-UX-05）。
 * ⚠️ 校验中禁用发送、失败显示原因并禁用发送（IMG-UX-02）。
 */
import { ArrowUp, CircleAlert, LoaderCircle, RotateCcw, X } from "@lucide/vue";
import { roleAllowsScope } from "~/utils/roleScope";
import {
  imageToolDisabledReason,
  imageToolPermissionBlocker,
  imageToolUnavailableBlocker,
  type ImageToolBlocker,
  type ImageToolCode
} from "~/utils/imageTools";
import { useImageToolsStore, type ImageToolQuote } from "~/stores/imageTools";

const props = withDefaults(defineProps<{
  title: string;
  /** 工具码：取 `/image-tools` 里这个工具的可用性（与工具条同一份缓存；方案变更后与源图尺寸无关）。 */
  tool: ImageToolCode;
  /** 能力校验（`useImageToolsStore().quotes[key]`）。 */
  quote?: ImageToolQuote;
  /** 编辑器自己的前置条件没满足时的说明（如「先选一个角色的情绪」），排在权限与不可用之后。 */
  inputHint?: string;
  /** 发送中（按钮转圈且不可再点）。 */
  sending?: boolean;
  /** 发送按钮的无障碍名。 */
  sendLabel?: string;
  /** 是否显示「重置参数」。 */
  resettable?: boolean;
}>(), {
  quote: undefined,
  inputHint: "",
  sending: false,
  sendLabel: "发送",
  resettable: true
});
const emit = defineEmits<{ close: []; reset: []; send: [] }>();

const workspace = useWorkspaceStore();
const imageTools = useImageToolsStore();

const role = computed(() => workspace.current?.role);
const canGenerate = computed(() => roleAllowsScope(role.value, "seeaitv:generate"));
const toolInfo = computed(() => imageTools.toolOf(props.tool));

const blockers = computed<Array<ImageToolBlocker | null>>(() => [
  imageToolPermissionBlocker(canGenerate.value, role.value === "viewer"),
  imageToolUnavailableBlocker(toolInfo.value),
  props.inputHint ? { kind: "input", text: props.inputHint, userFixable: true } : null,
  props.quote?.error ? { kind: "quote", text: props.quote.error } : null
]);
/** 最根本的那一条。 */
const reason = computed(() => imageToolDisabledReason(blockers.value));
const quoting = computed(() => Boolean(props.quote?.loading));
const disabled = computed(() => Boolean(reason.value) || quoting.value || props.sending || !props.quote?.ready);
const sendTitle = computed(() => {
  if (reason.value) return reason.value.text;
  if (quoting.value) return "正在检查模型能否使用…";
  if (!props.quote?.ready) return "还没检查模型能否使用";
  return props.sendLabel;
});

function send() {
  if (!disabled.value) emit("send");
}

defineExpose({ reason, disabled });
</script>

<template>
  <section class="node-param-panel tool-panel-shell nodrag nowheel" :aria-label="title" @pointerdown.stop @mousedown.stop>
    <header class="tool-panel-head">
      <strong>{{ title }}</strong>
      <div class="tool-panel-head-extra"><slot name="header" /></div>
      <button type="button" class="tool-panel-close" title="关闭（Esc）" aria-label="关闭" @click="emit('close')"><X :size="15" /></button>
    </header>
    <div class="tool-panel-body"><slot /></div>
    <footer class="node-bottom-controls tool-panel-foot">
      <button v-if="resettable" type="button" class="tool-panel-reset" @click="emit('reset')"><RotateCcw :size="13" />重置参数</button>
      <slot name="footer" />
      <p v-if="reason" class="tool-panel-reason" :class="`is-${reason.kind}`" role="status" data-testid="tool-panel-reason">
        <CircleAlert :size="13" /><span>{{ reason.text }}</span>
      </p>
      <!-- 原「预计积分」角标的位置（TT-T-14 删除金额）：保留空占位把发送键推到右侧，布局不变 -->
      <span class="node-estimate" aria-hidden="true" />
      <button class="node-generate" type="button" :disabled="disabled" :title="sendTitle" :aria-label="sendLabel" @click="send">
        <LoaderCircle v-if="sending" :size="17" class="spin" />
        <ArrowUp v-else :size="18" />
      </button>
    </footer>
  </section>
</template>

<style scoped>
.tool-panel-head { min-height: 44px; display: flex; align-items: center; gap: 10px; padding: 8px 10px 0 16px; color: var(--cv-n92); }
.tool-panel-head > strong { flex: 0 0 auto; font-size: 13px; font-weight: 600; }
.tool-panel-head-extra { min-width: 0; display: flex; flex: 1 1 auto; align-items: center; justify-content: flex-end; gap: 6px; }
.tool-panel-close { width: 28px; height: 28px; display: grid; flex: 0 0 auto; place-items: center; border: 0; border-radius: 6px; color: var(--cv-n63); background: transparent; cursor: pointer; }
.tool-panel-close:hover,
.tool-panel-close:focus-visible { color: var(--cv-n92); background: var(--cv-n21); }
.tool-panel-body { min-height: 120px; padding: 10px 16px; color: var(--cv-n88); font-size: 12px; }
.tool-panel-reset { height: 29px; display: inline-flex; flex: 0 0 auto; align-items: center; gap: 5px; border: 1px solid transparent; border-radius: 6px; padding: 0 9px; color: var(--cv-n70); background: var(--cv-n20); font-size: 11px; cursor: pointer; }
.tool-panel-reset:hover,
.tool-panel-reset:focus-visible { color: var(--cv-n92); background: var(--cv-n22); }
.tool-panel-reason { min-width: 0; display: flex; flex: 1 1 auto; align-items: center; justify-content: flex-end; gap: 5px; margin: 0; color: var(--cv-n63); font-size: 11px; line-height: 1.4; }
.tool-panel-reason > svg { flex: none; }
.tool-panel-reason > span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tool-panel-reason.is-quote { color: var(--canvas-danger); }
.tool-panel-reason + .node-estimate { margin-left: 0; }
</style>
