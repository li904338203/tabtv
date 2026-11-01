<script setup lang="ts">
/**
 * 从智能剪辑发布出来的成片节点上的来源角标（SCT-24，SCD-51，SC-UX-15）：
 * - 「来自 {智能剪辑名} · v{版本}」：点了定位到来源智能剪辑节点；来源已删除时说明「来源智能剪辑已删除」；
 * - 时间线比发布时新时补一句「剪辑已更新」：重新导出后旧发布节点**不自动替换**（用户可能正拿旧版去生成）；
 * - 「转为普通视频节点」：去掉来源，参数面板重新出现（同导演台的发布节点）。
 *
 * 同导演台的角标，挂两处（`part`）：`chip` 叠在画面左上角，总在；`panel` 是选中时卡片下方的操作区。
 */
import { Scissors, Unlink } from "@lucide/vue";
import type { JsonObject } from "~/types/canvas";
import { directorCanEdit } from "~/utils/directorRole";
import { SMARTCUT_NODE_TYPE, smartcutTimelineIdOf } from "~/utils/smartcutCanvas";
import { smartcutSourceBadge, smartcutSourceOf } from "~/utils/smartcutPublish";

const props = defineProps<{ nodeId: string; params: JsonObject; part: "chip" | "panel" }>();
const emit = defineEmits<{ "locate-smartcut": [smartcutNodeId: string] }>();

const canvas = useCanvasStore();
const workspace = useWorkspaceStore();
const smartcuts = useSmartcuts();

const source = computed(() => smartcutSourceOf(props.params));
/** 来源节点：画布上指向这条时间线的那个智能剪辑节点。被删了就找不到。 */
const sourceNode = computed(() => {
  const timelineId = source.value?.timelineId;
  if (!timelineId) return undefined;
  return canvas.nodes.find(node => node.data.nodeType === SMARTCUT_NODE_TYPE && smartcutTimelineIdOf(node.data.params) === timelineId);
});
const badge = computed(() => {
  if (!source.value) return null;
  const node = sourceNode.value;
  const summary = node ? smartcuts.get(node.id) : undefined;
  return smartcutSourceBadge(source.value, node ? { title: node.data.title, version: summary?.version } : null);
});
const canEdit = computed(() => directorCanEdit(workspace.current?.role));

function locate() {
  if (sourceNode.value) emit("locate-smartcut", sourceNode.value.id);
}

/** 去掉来源（可以撤销）：之后它就是一个普通的视频节点，可以重新生成。 */
function convertToPlain() {
  if (!canEdit.value) return;
  canvas.snapshot();
  canvas.markTouched(props.nodeId);
  const { smartcutSource: _source, ...rest } = props.params;
  canvas.updateParams(props.nodeId, rest);
}
</script>

<template>
  <template v-if="source && badge">
    <button
      v-if="part === 'chip'"
      type="button"
      class="smartcut-source-chip nodrag"
      :class="{ updated: badge.updated, orphan: badge.missing }"
      :title="badge.missing ? badge.text : `${badge.text}（点击定位来源）`"
      :aria-label="badge.text"
      data-testid="smartcut-source-chip"
      @mousedown.stop
      @click.stop="locate"
    >
      <Scissors :size="12" /><span>{{ badge.text }}</span>
    </button>
    <div v-else class="smartcut-source-panel nodrag nowheel" @mousedown.stop>
      <p class="smartcut-source-label"><Scissors :size="13" />{{ badge.text }}</p>
      <p v-if="badge.updated" class="smartcut-source-notice" role="status">剪辑在发布之后又改过：这个节点里还是发布时的那一版成片。要用新版，回到智能剪辑重新导出再发布。</p>
      <div class="smartcut-source-actions">
        <button type="button" :disabled="!canEdit" :title="canEdit ? '去掉来源，变回普通视频节点（可以撤销）' : '需要编辑权限：你在该工作区是只读成员'" @click="convertToPlain"><Unlink :size="13" />转为普通视频节点</button>
      </div>
    </div>
  </template>
</template>

<style scoped>
.smartcut-source-chip {
  position: absolute; left: 8px; top: 8px; z-index: 4; display: inline-flex; align-items: center; gap: 4px; max-width: calc(100% - 16px);
  padding: 3px 8px; border: 1px solid var(--cv-tint-22); border-radius: 999px; color: var(--cv-n92); background: var(--cv-n09a84); font-size: 11px; cursor: pointer;
}
.smartcut-source-chip span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.smartcut-source-chip.updated { border-color: var(--canvas-cyan); }
.smartcut-source-chip.orphan { cursor: default; opacity: 0.75; }
.smartcut-source-panel {
  position: relative; z-index: 9; display: grid; gap: 8px; margin-top: 9px; padding: 10px 12px; border: 1px solid var(--canvas-line); border-radius: 8px;
  color: var(--cv-n88); background: var(--cv-n15); font-size: 12px; box-shadow: 0 16px 34px var(--cv-shade-32);
}
.smartcut-source-panel p { margin: 0; }
.smartcut-source-label { display: flex; align-items: center; gap: 6px; color: var(--cv-n63); }
.smartcut-source-notice { color: var(--cv-n96); }
.smartcut-source-actions { display: flex; flex-wrap: wrap; gap: 6px; }
.smartcut-source-actions button {
  display: inline-flex; align-items: center; gap: 5px; padding: 4px 10px; border: 1px solid var(--cv-tint-22); border-radius: 6px;
  color: var(--cv-n90); background: transparent; font-size: 12px; cursor: pointer;
}
.smartcut-source-actions button:disabled { opacity: 0.45; cursor: not-allowed; }
</style>
