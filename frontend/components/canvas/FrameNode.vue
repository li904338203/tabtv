<script setup lang="ts">
/**
 * 画框（Vue Flow 节点类型 `frame`，AGT-11）：半透明底 + 左上角标题，可改名、可解散。
 *
 * ⚠️ 画框是**纯视觉容器**，与带执行与计费语义的组节点（group）严格区分：
 *    不连线、不参与整图运行（服务端解析时跳过 frame）、拖动不带动成员（成员位置独立）。
 *    解散 = 删掉画框节点本身，成员原样留在画布上（可撤销）。
 * ⚠️ 只有标题栏接收指针事件（见 CanvasBoard 的全局样式）：框内空白处要能照常框选 / 平移画布，
 *    否则一个铺满半屏的画框会把那片画布「锁住」。
 */
import type { NodeProps } from "@vue-flow/core";
import { Ungroup } from "@lucide/vue";
import OriginBadge from "./OriginBadge.vue";
import type { CanvasNodeData } from "~/types/canvas";

const props = defineProps<NodeProps<CanvasNodeData>>();
const canvas = useCanvasStore();

const editing = ref(false);
const draft = ref("");
const input = ref<HTMLInputElement>();

const size = computed(() => {
  const node = canvas.nodes.find(item => item.id === props.id);
  return { width: `${Number(node?.width) || 640}px`, height: `${Number(node?.height) || 400}px` };
});
const highlighted = computed(() => canvas.highlightedNodeIds.includes(props.id));

function startRename() {
  draft.value = props.data.title;
  editing.value = true;
  void nextTick(() => input.value?.select());
}

function commitRename() {
  if (!editing.value) return;
  editing.value = false;
  canvas.updateTitle(props.id, draft.value);
}

function cancelRename() {
  editing.value = false;
}

function dissolve() {
  canvas.removeNode(props.id);
}
</script>

<template>
  <section class="frame-node" :class="{ selected, 'remote-new': highlighted }" :style="size" :aria-label="`画框：${data.title}`">
    <header class="frame-node-heading">
      <OriginBadge :source="data" />
      <input
        v-if="editing"
        ref="input"
        v-model="draft"
        class="frame-node-title-input nodrag"
        maxlength="60"
        aria-label="画框名称"
        @keyup.enter="commitRename"
        @keyup.esc="cancelRename"
        @blur="commitRename"
      >
      <strong v-else title="双击改名" @dblclick.stop="startRename">{{ data.title }}</strong>
      <button type="button" class="nodrag" title="解散画框（框内节点保留）" aria-label="解散画框" @click.stop="dissolve"><Ungroup :size="13" /></button>
    </header>
  </section>
</template>

<style scoped>
.frame-node {
  position: relative;
  box-sizing: border-box;
  border: 1px dashed var(--canvas-line-strong);
  border-radius: 14px;
  background: var(--cv-cyan-52a7);
}
.frame-node.selected { border-style: solid; border-color: var(--canvas-cyan); }
.frame-node-heading {
  position: absolute;
  top: 0;
  left: 0;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  max-width: 100%;
  padding: 6px 10px;
  color: var(--canvas-text);
  font-size: 12px;
  pointer-events: auto;
  cursor: grab;
}
.frame-node-heading strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 600; }
.frame-node-title-input {
  width: 180px;
  padding: 2px 6px;
  border: 1px solid var(--canvas-line-strong);
  border-radius: 6px;
  background: var(--canvas-surface);
  color: var(--canvas-text);
  font: inherit;
}
.frame-node-heading button {
  display: inline-grid;
  place-items: center;
  width: 20px;
  height: 20px;
  padding: 0;
  border: 0;
  border-radius: 5px;
  background: transparent;
  color: var(--canvas-muted);
  cursor: pointer;
  opacity: 0;
}
.frame-node:hover .frame-node-heading button,
.frame-node.selected .frame-node-heading button,
.frame-node-heading button:focus-visible { opacity: 1; }
.frame-node-heading button:hover { color: var(--canvas-text); background: var(--canvas-raised); }
</style>
