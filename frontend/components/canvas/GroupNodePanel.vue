<script setup lang="ts">
import { Download, Layers, LayoutGrid, LoaderCircle, Play, RefreshCw, Scissors, Square } from "@lucide/vue";
import { ElMessage } from "element-plus";
import type { CanvasNodeData } from "~/types/canvas";
import type { GroupDetail, GroupItem } from "~/types/script";
import { extensionFor, safeFilename, saveBlob } from "~/utils/download";
import type { GroupResp } from "~/utils/scriptContract";

const props = defineProps<{ nodeId: string; data: CanvasNodeData }>();
const emit = defineEmits<{
  update: [group: GroupDetail];
  ungroup: [group: GroupDetail];
}>();
const group = ref<GroupDetail>();
const busy = ref("");
/** 组接口只走脚本接口的统一入口（新契约，见 utils/scriptContract）。 */
const scripts = useScriptApi();
const canvas = useCanvasStore();
const media = useAuthenticatedMedia();
/** 执行用的模型：新后端组上不存模型，建组时选的记在画布节点上。 */
const modelCode = computed(() => String(props.data.params.modelCode ?? ""));
let refreshTimer: ReturnType<typeof setTimeout> | undefined;
const groupId = computed(() => String(props.data.params.groupId ?? ""));
const canExecute = computed(() => group.value?.groupType === "video_generator"
  && group.value.items.some(item => ["draft", "failed", "cancelled"].includes(item.status)));
const hasRunning = computed(() => group.value?.items.some(item => ["queued", "running"].includes(item.status)));
const hasFailed = computed(() => group.value?.items.some(item => item.status === "failed"));
const totalCount = computed(() => group.value?.items.length ?? 0);
const completedCount = computed(() => group.value?.items.filter(item => item.status === "success").length ?? 0);
const downloadableItems = computed(() => (group.value?.items ?? [])
  .filter(item => item.status === "success" && String(item.outputUrl ?? "").trim()));

/**
 * 逐个触发浏览器下载：产物是各自独立的 URL，没有打包接口，
 * 前端不引入 zip 依赖，按顺序间隔触发以免被浏览器拦截。
 */
async function downloadAll() {
  if (busy.value || !downloadableItems.value.length) return;
  busy.value = "download";
  try {
    const items = downloadableItems.value;
    let saved = 0;
    for (const [index, item] of items.entries()) {
      const name = String((item.paramsSnapshot ?? {}).name ?? "")
        || String((item.paramsSnapshot ?? {}).shotNo ?? "")
        || String(index + 1);
      // ⚠️ 产物在受保护端点上：<a href> 不带鉴权头，直链一律 401（GAP-18）。先取成 blob 再存
      const response = await media.fetchMedia(String(item.outputUrl));
      if (!response.ok) continue;
      const blob = await response.blob();
      saveBlob(blob, `${safeFilename(`${group.value?.name ?? "group"}-${name}`)}.${extensionFor(blob.type)}`);
      saved += 1;
      if (index < items.length - 1) await new Promise(resolve => setTimeout(resolve, 220));
    }
    if (saved === items.length) ElMessage.success(`已开始下载 ${saved} 个产物`);
    else ElMessage.warning(`已下载 ${saved}/${items.length} 个产物，其余的暂时取不到`);
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "批量下载失败");
  } finally {
    busy.value = "";
  }
}

/**
 * 上一次已知的组（刷新时沿用镜号 / 资产名 / 产物地址：新后端的子项不带这些）。
 * 节点上没有 groupId 时（早先被写坏的节点）`refreshGroup` 按 `projectId` + `canvasNodeId` 取，所以这两项一定要给。
 */
function previousGroup(): GroupDetail {
  if (group.value) return group.value;
  const items = Array.isArray(props.data.params.items) ? props.data.params.items as GroupItem[] : [];
  return {
    groupId: groupId.value,
    projectId: String(props.data.params.projectId ?? canvas.projectId),
    scriptId: String(props.data.params.scriptId ?? ""),
    canvasNodeId: props.nodeId,
    groupType: String(props.data.params.groupType ?? ""),
    name: props.data.title,
    status: String(props.data.params.groupStatus ?? "draft"),
    version: Number(props.data.params.groupVersion ?? 1),
    items
  };
}

async function adopt(resp: GroupResp) {
  apply(await scripts.adoptGroup(resp, previousGroup()));
}

function apply(value: GroupDetail) {
  group.value = { ...value, modelCode: modelCode.value || value.modelCode };
  emit("update", value);
  clearTimeout(refreshTimer);
  if (value.items.some(item => ["queued", "running"].includes(item.status))) {
    refreshTimer = setTimeout(() => void load(), 1500);
  }
}

async function load() {
  if (busy.value) return;
  busy.value = "refresh";
  try {
    apply(await scripts.refreshGroup(previousGroup()));
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "组节点刷新失败");
  } finally {
    busy.value = "";
  }
}

async function action(name: "execute" | "retry" | "cancel" | "ungroup") {
  if (!group.value || busy.value) return;
  const current = group.value;
  // ⚠️ 新后端执行 / 重试都要带模型（组上不存模型）：没有记录就说清楚，不要发一个必然 422 的请求
  if ((name === "execute" || name === "retry") && !modelCode.value) {
    ElMessage.warning("这个组没有记下执行用的模型，请在脚本工作台里重新创建");
    return;
  }
  busy.value = name;
  try {
    if (name === "ungroup") {
      // 解组只回每个子项落到画布上的节点 ID，节点内容沿用当前组里的
      const result = await scripts.ungroup(current.groupId, current.version);
      const nodeIds = new Map((result.nodes ?? []).map(node => [node.itemId, node.nodeId]));
      const next = {
        ...current,
        status: result.status,
        version: result.version,
        items: current.items.map(item => ({ ...item, canvasNodeId: nodeIds.get(item.itemId) ?? item.canvasNodeId }))
      };
      group.value = next;
      emit("ungroup", next);
      return;
    }
    if (name === "cancel") {
      await adopt(await scripts.cancelGroup(current.groupId));
    } else if (name === "retry") {
      // 新后端没有「整组重试」：把失败的子项点名重跑（每项会是一次新任务、重新计费）
      const failed = current.items.filter(item => item.status === "failed").map(item => item.itemId);
      await adopt(await scripts.executeGroup(current.groupId, {
        modelCode: modelCode.value, version: current.version, itemIds: failed, force: true
      }));
    } else {
      await adopt(await scripts.executeGroup(current.groupId, { modelCode: modelCode.value, version: current.version }));
    }
    ElMessage.success(name === "execute" ? "视频组已开始执行" : name === "retry" ? "失败项已重新提交" : "运行项已停止");
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "组节点操作失败");
  } finally {
    busy.value = "";
  }
}

onMounted(load);
onBeforeUnmount(() => clearTimeout(refreshTimer));
</script>

<template>
  <!-- 悬浮在组节点上方的横条：早先是节点下方的面板，会把画布纵向撑长 -->
  <div class="group-node-commandbar nodrag nopan nowheel" @pointerdown.stop @mousedown.stop>
    <span class="group-commandbar-avatar" :title="group?.name ?? data.title"><Layers :size="16" /></span>
    <span class="group-commandbar-count" :title="`${completedCount}/${totalCount} 已完成`">
      <LayoutGrid :size="16" /><em>{{ completedCount }}/{{ totalCount }}</em>
    </span>
    <span class="group-commandbar-divider" />
    <button type="button" :disabled="Boolean(busy)" title="刷新组状态" @click="load">
      <LoaderCircle v-if="busy === 'refresh'" :size="15" class="spin" /><RefreshCw v-else :size="15" />
      <span class="sr-only">刷新组状态</span>
    </button>
    <button v-if="canExecute" type="button" :disabled="Boolean(busy)" title="整组执行" @click="action('execute')">
      <LoaderCircle v-if="busy === 'execute'" :size="15" class="spin" /><Play v-else :size="15" /><span>整组执行</span>
    </button>
    <button v-if="hasFailed" type="button" :disabled="Boolean(busy)" title="重试失败项" @click="action('retry')">
      <LoaderCircle v-if="busy === 'retry'" :size="15" class="spin" /><RefreshCw v-else :size="15" /><span>重试失败项</span>
    </button>
    <button v-if="hasRunning" type="button" :disabled="Boolean(busy)" title="停止运行项" @click="action('cancel')">
      <LoaderCircle v-if="busy === 'cancel'" :size="15" class="spin" /><Square v-else :size="14" /><span>停止运行项</span>
    </button>
    <button type="button" :disabled="Boolean(busy)" title="解组" @click="action('ungroup')">
      <LoaderCircle v-if="busy === 'ungroup'" :size="15" class="spin" /><Scissors v-else :size="15" /><span>解组</span>
    </button>
    <button
      v-if="downloadableItems.length"
      type="button"
      :disabled="Boolean(busy)"
      :title="`批量下载 ${downloadableItems.length} 个产物`"
      @click="downloadAll"
    >
      <LoaderCircle v-if="busy === 'download'" :size="15" class="spin" /><Download v-else :size="15" /><span>批量下载</span>
    </button>
  </div>
</template>
