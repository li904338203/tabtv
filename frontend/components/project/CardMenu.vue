<script setup lang="ts">
/**
 * 项目卡片的「⋯」菜单 —— 工作台、AI 视频页、全部项目页**共用这一个**。
 *
 * ⚠️ 只负责「长什么样、点了发什么事件」，**不自己调接口**：三个页面的列表来源不同
 *    （/home、/projects?limit=6、滚动分页），操作完各自要刷新各自的列表。
 *    接口与确认文案在 `useProjectActions` 里，也只有那一份。
 *
 * 样式沿用全部项目页的 `.library-actions*` —— 三处看起来一样，是因为它们**就是**同一套。
 */
import type { components as ApiComponents } from "@tabtv/api-client";
import { ChevronRight, Copy, Download, FolderClosed, Inbox, LayoutDashboard, MoreHorizontal, Pencil, SquareArrowOutUpRight, Trash2 } from "@lucide/vue";
import type { ProjectCardRef } from "~/composables/useProjectActions";
import { folderMenuMode } from "~/utils/projectMenu";

type Folder = ApiComponents["schemas"]["Folder"];

const props = defineProps<{
  project: ProjectCardRef;
  folders: Folder[];
  /**
   * Agent 项目（AGT-45）：「打开」进的是对话页，另给一项「打开画布」。
   * 由调用方按列表项的 `agent` 字段传，菜单自己不判类型。
   */
  agent?: boolean;
  /** 显示「导出项目包」（TT-T-33）。只有全部项目页开着：首页与 AI 视频页的菜单保持原样。 */
  exportable?: boolean;
}>();
const emit = defineEmits<{
  open: [];
  /** 只在 `agent` 时出现：打开画布（面板展开在最近会话上）。 */
  openCanvas: [];
  rename: [];
  duplicate: [];
  /** 只在 `exportable` 时出现：导出项目包。 */
  export: [];
  move: [folderId: string];
  remove: [];
  /** 菜单第一次展开时发出：调用方可以在这时才去取文件夹列表（首页不必一进来就取）。 */
  opened: [];
}>();

const root = ref<HTMLElement>();
const visible = ref(false);
const moveVisible = ref(false);
/** 子菜单往左弹：卡片在最右一列时往右弹会冲出屏幕（首页是 6 列窄卡片）。 */
const moveFlip = ref(false);

/** 「移出」还是「移动至」—— 判定在 utils/projectMenu 里单测（按项目自己的 folderId 判）。 */
const folderMode = computed(() => folderMenuMode(props.project.folderId, props.folders.length));

function show() {
  if (visible.value) return;
  visible.value = true;
  emit("opened");
}
function hide() {
  visible.value = false;
  moveVisible.value = false;
}
function toggle() {
  if (visible.value) hide();
  else show();
}
function showMove() {
  const rect = root.value?.getBoundingClientRect();
  // 子菜单约 150px 宽；右边放不下就改往左弹
  moveFlip.value = Boolean(rect && rect.right + 170 > window.innerWidth);
  moveVisible.value = true;
}

/** 选中一项：先关菜单再发事件 —— 反过来的话确认弹窗弹出时菜单还挂在上面。 */
function pick(action: () => void) {
  hide();
  action();
}

// 点空白处关菜单：只靠再点一次触发器关，用户会觉得菜单「粘住了」
onMounted(() => document.addEventListener("click", hide));
onBeforeUnmount(() => document.removeEventListener("click", hide));
</script>

<template>
  <div ref="root" class="library-actions" @click.stop @mouseenter="show" @mouseleave="hide">
    <button
      type="button"
      class="library-actions-trigger"
      :title="`${project.name} 的操作`"
      :aria-label="`${project.name} 的操作`"
      :aria-expanded="visible"
      @click="toggle"
    ><MoreHorizontal :size="16" /></button>
    <div v-if="visible" class="library-actions-menu" role="menu">
      <button type="button" role="menuitem" @click="pick(() => emit('open'))">
        <SquareArrowOutUpRight :size="15" />{{ agent ? "打开对话" : "打开" }}
      </button>
      <button v-if="agent" type="button" role="menuitem" @click="pick(() => emit('openCanvas'))">
        <LayoutDashboard :size="15" />打开画布
      </button>
      <button type="button" role="menuitem" @click="pick(() => emit('rename'))">
        <Pencil :size="15" />重命名
      </button>
      <button type="button" role="menuitem" @click="pick(() => emit('duplicate'))">
        <Copy :size="15" />创建副本
      </button>
      <button v-if="exportable" type="button" role="menuitem" @click="pick(() => emit('export'))">
        <Download :size="15" />导出项目包
      </button>
      <!-- 已在文件夹里只给「移出」：再列一遍文件夹、选回它自己所在的那个是空操作 -->
      <button
        v-if="folderMode === 'move-out'"
        type="button"
        role="menuitem"
        @click="pick(() => emit('move', ''))"
      ><Inbox :size="15" />移出文件夹</button>
      <div
        v-else-if="folderMode === 'move-into'"
        class="library-move"
        @mouseenter="showMove"
        @mouseleave="moveVisible = false"
      >
        <button type="button" role="menuitem" class="library-move-trigger" @click="showMove">
          <FolderClosed :size="15" />移动至文件夹<ChevronRight :size="14" class="library-move-arrow" />
        </button>
        <div v-if="moveVisible" class="library-move-panel" :class="{ flip: moveFlip }" role="menu">
          <button
            v-for="folder in folders"
            :key="folder.id"
            type="button"
            role="menuitem"
            @click="pick(() => emit('move', folder.id ?? ''))"
          ><FolderClosed :size="15" />{{ folder.name }}</button>
        </div>
      </div>
      <button type="button" role="menuitem" class="danger" @click="pick(() => emit('remove'))">
        <Trash2 :size="15" />删除项目
      </button>
    </div>
  </div>
</template>
