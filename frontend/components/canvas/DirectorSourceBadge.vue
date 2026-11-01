<script setup lang="ts">
/**
 * 从 3D 导演台发布出来的节点上的来源角标（DRT-17，dev/03 §7.2）：
 * - 角标「来自 3D 导演台 · {机位名}」：点了定位到来源导演台；来源已删除时说明（DR-UX-15）；
 * - 过期提示：节点上那条记录的指纹 ≠ 来源场景当前的指纹（DR-F-83，前端不算指纹，只比较）；
 * - 「重新截图」：打开来源导演台、截同一个机位，截完**只替换这个节点**（DR-F-84）；
 * - 「转为普通图片 / 视频节点」：去掉来源，参数面板重新出现（预演是视频节点，DRT-25）。
 * 只读成员看得到角标与过期提示，两个按钮置灰并说明原因（DR-F-85）。
 *
 * 同一个组件挂两处（`part`）：`chip` 是叠在画面左上角的角标，总在；`panel` 是选中时卡片下方的操作区
 * （参数面板的位置 —— 发布节点是静态素材，不显示参数面板，DR-F-81）。
 */
import { Clapperboard, RefreshCw, Unlink } from "@lucide/vue";
import type { JsonObject } from "~/types/canvas";
import { DIRECTOR_NODE_TYPE, directorSceneIdOf } from "~/utils/directorCanvas";
import { directorConvertToPlain, directorKindNote, directorSourceOf } from "~/utils/directorPublish";
import { directorCanEdit } from "~/utils/directorRole";
import { directorPublishedStale, directorStaleText } from "~/utils/directorStale";

const props = defineProps<{ nodeId: string; params: JsonObject; part: "chip" | "panel" }>();
const emit = defineEmits<{
  /** 定位到来源导演台（传来源导演台的节点 ID）。 */
  "locate-director": [directorNodeId: string];
  /** 重新截图：打开来源导演台，截 `cameraId`，截完替换本节点。 */
  recapture: [directorNodeId: string, cameraId: string | null];
}>();

const canvas = useCanvasStore();
const workspace = useWorkspaceStore();
const scenes = useDirectorScenes();

const source = computed(() => directorSourceOf(props.params));
/** 来源导演台节点：画布上 `sceneId` 指向这个场景的那个导演台。被删了就找不到。 */
const directorNode = computed(() => {
  const sceneId = source.value?.sceneId;
  if (!sceneId) return undefined;
  return canvas.nodes.find(node => node.data.nodeType === DIRECTOR_NODE_TYPE && directorSceneIdOf(node.data.params) === sceneId);
});
const stale = computed(() => directorPublishedStale(source.value, source.value ? scenes.summaries.value[source.value.sceneId] : undefined));
const preview = computed(() => source.value?.kind === "preview");
const notice = computed(() => directorStaleText({ stale: stale.value, sourceDeleted: !directorNode.value, preview: preview.value }));
const canEdit = computed(() => directorCanEdit(workspace.current?.role));
const label = computed(() => `来自 3D 导演台 · ${source.value?.cameraName || "机位"}`);
/** 深度图 / 骨架图注明类型（DR-F-134）；它们的「重新截图」要回到导演台里截，这里不给（会被替换成一张普通截图）。 */
const kindNote = computed(() => (source.value ? directorKindNote(source.value.kind) : ""));
const controlMap = computed(() => source.value?.kind === "depth" || source.value?.kind === "pose");
/** 角标上机位名后面的类型：深度图 / 骨架图 / 预演；普通截图不加。 */
const chipKind = computed(() => ({ depth: "深度图", pose: "骨架图", preview: "预演" } as Record<string, string>)[source.value?.kind ?? ""] ?? "");
const plainLabel = computed(() => (preview.value ? "转为普通视频节点" : "转为普通图片节点"));
const recaptureDisabledReason = computed(() => {
  if (!canEdit.value) return "需要编辑权限：你在该工作区是只读成员";
  if (!directorNode.value) return "来源导演台已删除";
  return "";
});

function locate() {
  if (directorNode.value) emit("locate-director", directorNode.value.id);
}

function recapture() {
  if (recaptureDisabledReason.value || !directorNode.value) return;
  emit("recapture", directorNode.value.id, source.value?.cameraId ?? null);
}

function convertToPlain() {
  if (!canEdit.value) return;
  canvas.snapshot();
  canvas.markTouched(props.nodeId);
  canvas.updateParams(props.nodeId, directorConvertToPlain(props.params));
}
</script>

<template>
  <template v-if="source">
    <!-- ⚠️ `.stop`：点角标不该同时触发卡片的选中 / 拖动 -->
    <button
      v-if="part === 'chip'"
      type="button"
      class="director-source-chip nodrag"
      :class="{ stale, orphan: !directorNode }"
      :title="directorNode ? `${label}（点击定位来源）` : `${label}（来源导演台已删除）`"
      :aria-label="label"
      @mousedown.stop
      @click.stop="locate"
    >
      <Clapperboard :size="12" /><span>{{ source.cameraName || "机位" }}{{ chipKind ? ` · ${chipKind}` : "" }}</span><i v-if="stale" aria-hidden="true" />
    </button>
    <div v-else class="director-source-panel nodrag nowheel" @mousedown.stop>
      <p class="director-source-label"><Clapperboard :size="13" />{{ label }}</p>
      <p v-if="kindNote" class="director-source-kind" data-testid="director-kind-note">{{ kindNote }}</p>
      <p v-if="notice" class="director-source-notice" :class="{ stale }" role="status">{{ notice.text }}</p>
      <div class="director-source-actions">
        <button
          v-if="notice?.action === 'recapture' && !controlMap"
          type="button"
          :disabled="Boolean(recaptureDisabledReason)"
          :title="recaptureDisabledReason || '打开来源导演台，截同一个机位，截完只替换这个节点'"
          @click="recapture"
        ><RefreshCw :size="13" />重新截图</button>
        <button
          type="button"
          :disabled="!canEdit"
          :title="canEdit ? `去掉来源，变回普通${preview ? '视频' : '图片'}节点（可以撤销）` : '需要编辑权限：你在该工作区是只读成员'"
          @click="convertToPlain"
        ><Unlink :size="13" />{{ plainLabel }}</button>
      </div>
      <p v-if="!canEdit" class="director-source-hint">只读成员可以查看来源与过期状态，不能重新截图或转换</p>
    </div>
  </template>
</template>

<style scoped>
.director-source-chip {
  position: absolute;
  left: 8px;
  top: 8px;
  z-index: 4;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  max-width: calc(100% - 16px);
  padding: 3px 8px;
  border: 1px solid var(--cv-tint-22);
  border-radius: 999px;
  color: var(--cv-n92);
  background: var(--cv-n09a84);
  font-size: 11px;
  cursor: pointer;
}
.director-source-chip span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.director-source-chip i { width: 6px; height: 6px; border-radius: 50%; background: var(--canvas-cyan); }
.director-source-chip.orphan { cursor: default; opacity: 0.75; }
.director-source-panel {
  position: relative;
  z-index: 9;
  display: grid;
  gap: 8px;
  margin-top: 9px;
  padding: 10px 12px;
  border: 1px solid var(--canvas-line);
  border-radius: 8px;
  color: var(--cv-n88);
  background: var(--cv-n15);
  font-size: 12px;
  box-shadow: 0 16px 34px var(--cv-shade-32);
}
.director-source-panel p { margin: 0; }
.director-source-kind { color: var(--cv-n80); }
.director-source-label { display: flex; align-items: center; gap: 6px; color: var(--cv-n63); }
.director-source-notice.stale { color: var(--cv-n96); }
.director-source-actions { display: flex; flex-wrap: wrap; gap: 6px; }
.director-source-actions button {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 4px 10px;
  border: 1px solid var(--cv-tint-22);
  border-radius: 6px;
  color: var(--cv-n90);
  background: transparent;
  font-size: 12px;
  cursor: pointer;
}
.director-source-actions button:hover:not(:disabled) { background: var(--cv-n21); }
.director-source-actions button:disabled { opacity: 0.45; cursor: not-allowed; }
.director-source-hint { color: var(--cv-n50); font-size: 11px; }
</style>
