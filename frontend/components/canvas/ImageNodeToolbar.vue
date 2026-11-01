<script setup lang="ts">
import type { components } from '~/types/legacy';
import {
  Aperture, Box, ChevronDown, Clock, Download, Film, Globe, Grid2x2, Grid3x3, Images,
  Layers, LibraryBig, Maximize2, Orbit, Package, PersonStanding,
  ScanFace, Scissors, Smile, SquareUserRound, SunDim, Table2, UserRound, Wand2
} from "@lucide/vue";
import { ElMessage } from "element-plus";
import { useImageToolsStore } from "~/stores/imageTools";
import { isPickerPreset, presetCodeOf, presetsInMenuOrder } from "~/utils/canvas";
import {
  imageToolDefaultOptions,
  imageToolIsNew,
  imageToolLabel,
  imageToolPresetLookup,
  imageToolsConfigOf,
  imageToolUnavailableBlocker,
  type ImageToolCode
} from "~/utils/imageTools";

type PresetItem = components["schemas"]["PresetResp"];

const props = defineProps<{
  /** 本节点（源图节点）的 ID：「高清」从它派生子节点。 */
  nodeId: string;
  presets: PresetItem[];
  /** 源图（本节点产物）的媒体 ID：情绪调节的默认选项要记下它（IMG-D-03）；可用性与单价与源图无关。 */
  sourceMediaId?: string;
  /** 当前打开着的编辑器（高亮对应按钮；再点一次关闭由父组件处理）。 */
  activeEditor?: string;
}>();
const emit = defineEmits<{
  select: [value: PresetItem];
  preview: [];
  /** 打开 / 关闭编辑器（情绪调节 / 多角度 / 打光，IMG-F-04）。 */
  "toggle-editor": [tool: ImageToolCode];
}>();

const configStore = useCanvasConfigStore();
const imageTools = useImageToolsStore();
const root = ref<HTMLElement>();
const openMenu = ref("");

/**
 * 「宫格切分 ▾」= 九宫格里的「25宫格连贯分镜」与「多机位九宫格」两个玩法（2026-10-31 用户要求），走同一个 addPresetNode。
 * 涂抹重绘 / 抠图 / 下载仍是占位（本轮不改，IMG-08）。
 * 占位不做成禁用态：禁用会让人以为是当前状态不满足条件（比如没选中图），
 * 而实际是功能还没做 —— 点一下明确告知更省事。
 */
const PLACEHOLDER = "该功能尚未开放";

const PRESET_ICONS: Record<string, unknown> = {
  "library-big": LibraryBig,
  film: Film,
  "grid-3x3": Grid3x3,
  "grid-2x2": Grid2x2,
  "clock-3": Clock,
  "clock-5": Clock,
  "person-standing": PersonStanding,
  aperture: Aperture,
  globe: Globe,
  "scan-face": ScanFace,
  "user-round": UserRound,
  "square-user-round": SquareUserRound,
  layers: Layers,
  package: Package,
  box: Box
};

function presetIcon(item: PresetItem) {
  return PRESET_ICONS[item.icon ?? ""] ?? Images;
}

/**
 * 工具条按截图罗列所有玩法入口；顺序与竞品一致，避免用户在两边形成不同的肌肉记忆。
 * ⚠️ 与「图片预设」弹窗同一份判据（isPickerPreset）：`tool` 组的加工工具不进这里。
 */
const gridPresets = computed(() => presetsInMenuOrder(props.presets.filter(isPickerPreset)));

/** 按钮数据来自 `/canvas/config.imageTools`（IMGD-20）；缺字段时用兜底表。 */
const toolsConfig = computed(() => imageToolsConfigOf(configStore.config?.imageTools));
/** 人物调节 / 全景复用九宫格的预设（同一个 addPresetNode，IMG-F-02/03）；下线了照样显示并说明（IMG-F-06）。 */
const portrait = computed(() => imageToolPresetLookup(props.presets, toolsConfig.value.portraitPreset, presetCodeOf));
const panorama = computed(() => imageToolPresetLookup(props.presets, toolsConfig.value.panoramaPreset, presetCodeOf));
/**
 * 「宫格切分 ▾」的两项：直接调用九宫格里的两个玩法（2026-10-31 用户要求），名字也用预设自己的名字，
 * 与九宫格菜单一致（运营改名两处一起变）；下线了取不到预设时才用这里的兜底名。
 * 下线了照样列出并说明（同人物调节 / 全景，IMG-F-06）。
 */
const GRID_SPLIT_ENTRIES = [
  { code: "storyboard_25grid", fallback: "25宫格连贯分镜" },
  { code: "multicam_9grid", fallback: "多机位九宫格" }
] as const;
const gridSplit = computed(() => GRID_SPLIT_ENTRIES.map((entry) => {
  const found = imageToolPresetLookup(props.presets, entry.code, presetCodeOf);
  return { ...entry, ...found, label: found.preset?.name ?? entry.fallback };
}));

/** 工具的名称、NEW 角标与（服务端判定的）不可用原因。 */
function toolEntry(tool: ImageToolCode) {
  const info = imageTools.toolOf(tool);
  const blocker = imageToolUnavailableBlocker(info);
  return {
    name: imageToolLabel(toolsConfig.value, tool).name,
    isNew: imageToolIsNew(toolsConfig.value, tool),
    blocked: blocker?.text ?? "",
    userFixable: blocker?.userFixable ?? true
  };
}
const expression = computed(() => toolEntry("expression_edit"));
const multiAngle = computed(() => toolEntry("multi_angle"));
const relight = computed(() => toolEntry("relight"));
const upscale = computed(() => toolEntry("upscale"));
/** 「人像质感调节 ▾」自己的 NEW：由下拉里的新工具（情绪调节）的配置决定，不再借九宫格预设的 isNew（IMG-F-01）。 */
const portraitMenuNew = computed(() => expression.value.isNew);

function toggle(menu: string) {
  openMenu.value = openMenu.value === menu ? "" : menu;
}
function choose(item: PresetItem) {
  emit("select", item);
  openMenu.value = "";
}
function choosePreset(entry: { preset?: PresetItem; reason: string | null }) {
  openMenu.value = "";
  if (entry.preset) emit("select", entry.preset);
  else ElMessage.info(entry.reason ?? "该玩法已下线");
}
function toggleEditor(tool: ImageToolCode) {
  openMenu.value = "";
  emit("toggle-editor", tool);
}
/**
 * 「高清」：直接建子节点、只建不生成（IMG-F-05、IMGK-5）；子节点下方是高清面板（模型 + 增强程度，默认「标准」）。
 * ⚠️ 放在工具条里而不是 BaseNode：工具条是异步组件，工具 store 与这段逻辑跟着它延迟加载，
 * 不进画布页首屏（画布页包体有上限，见 scripts/director-bundle-check.mjs）。
 */
async function spawnTool(tool: ImageToolCode) {
  openMenu.value = "";
  const config = imageToolsConfigOf(configStore.config?.imageTools);
  const info = imageTools.toolOf(tool);
  await useCanvasStore().spawnToolNode(props.nodeId, tool,
    imageToolDefaultOptions(tool, { config, sourceMediaId: props.sourceMediaId }), {
      title: tool === "upscale" ? "高清" : imageToolLabel(config, tool).spawn,
      generate: false,
      model: info?.models?.[0]?.code
    });
}
function placeholder() {
  openMenu.value = "";
  ElMessage.info(PLACEHOLDER);
}
function titleOf(name: string, blocked: string) {
  return blocked ? `${name}：${blocked}` : name;
}
function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) openMenu.value = "";
}
function onKeydown(event: KeyboardEvent) {
  if (event.key === "Escape" && openMenu.value) {
    openMenu.value = "";
    event.stopPropagation();
  }
}

/**
 * 可用性与单价：打开工具条时取一次 `/image-tools`（store 有缓存；方案变更后不再按源图尺寸区分）。
 * 失败不打扰用户 —— 按钮照样能点，编辑器里会再说明。
 */
onMounted(() => {
  void imageTools.tools().catch(() => undefined);
});
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
</script>

<template>
  <div ref="root" class="image-node-toolbar nodrag nopan nowheel" @pointerdown.stop @mousedown.stop @keydown="onKeydown">
    <div class="toolbar-menu">
      <button
        type="button"
        class="toolbar-entry"
        :class="{ active: openMenu === 'portrait' || activeEditor === 'expression_edit' }"
        aria-haspopup="menu"
        :aria-expanded="openMenu === 'portrait'"
        @click.stop="toggle('portrait')"
      >
        <PersonStanding :size="15" /><span>人像质感调节</span>
        <i v-if="portraitMenuNew" class="toolbar-badge">NEW</i>
        <ChevronDown :size="12" />
      </button>
      <div v-if="openMenu === 'portrait'" class="toolbar-dropdown" role="menu">
        <button
          type="button"
          role="menuitem"
          :class="{ 'is-blocked': portrait.reason }"
          :title="portrait.reason ? `人物调节：${portrait.reason}` : '人物调节'"
          @click.stop="choosePreset(portrait)"
        >
          <UserRound :size="17" /><b>人物调节</b>
          <small v-if="portrait.reason">{{ portrait.reason }}</small>
        </button>
        <button
          type="button"
          role="menuitem"
          :class="{ 'is-blocked': expression.blocked }"
          :title="titleOf(expression.name, expression.blocked)"
          @click.stop="toggleEditor('expression_edit')"
        >
          <Smile :size="17" /><b>{{ expression.name }}</b>
          <i v-if="expression.isNew" />
        </button>
      </div>
    </div>
    <button
      type="button"
      class="toolbar-entry"
      :class="{ 'is-blocked': panorama.reason }"
      :title="panorama.reason ? `全景：${panorama.reason}` : '全景'"
      @click.stop="choosePreset(panorama)"
    >
      <Globe :size="15" /><span>全景</span>
    </button>
    <button
      type="button"
      class="toolbar-entry"
      :class="{ active: activeEditor === 'multi_angle', 'is-blocked': multiAngle.blocked }"
      :title="titleOf(multiAngle.name, multiAngle.blocked)"
      :aria-pressed="activeEditor === 'multi_angle'"
      @click.stop="toggleEditor('multi_angle')"
    >
      <Orbit :size="15" /><span>{{ multiAngle.name }}</span>
      <i v-if="multiAngle.isNew" class="toolbar-badge">NEW</i>
    </button>
    <button
      type="button"
      class="toolbar-entry"
      :class="{ active: activeEditor === 'relight', 'is-blocked': relight.blocked }"
      :title="titleOf(relight.name, relight.blocked)"
      :aria-pressed="activeEditor === 'relight'"
      @click.stop="toggleEditor('relight')"
    >
      <SunDim :size="15" /><span>{{ relight.name }}</span>
      <i v-if="relight.isNew" class="toolbar-badge">NEW</i>
    </button>

    <span class="toolbar-split" />

    <div class="toolbar-menu">
      <button
        type="button"
        class="toolbar-entry"
        :class="{ active: openMenu === 'grid' }"
        aria-haspopup="menu"
        :aria-expanded="openMenu === 'grid'"
        @click.stop="toggle('grid')"
      >
        <Grid3x3 :size="15" /><span>九宫格</span><ChevronDown :size="12" />
      </button>
      <div v-if="openMenu === 'grid'" class="toolbar-dropdown" role="menu">
        <button
          v-for="item in gridPresets"
          :key="presetCodeOf(item)"
          type="button"
          role="menuitem"
          @click.stop="choose(item)"
        >
          <component :is="presetIcon(item)" :size="17" />
          <b>{{ item.name }}</b>
          <i v-if="item.isNew" />
        </button>
        <p v-if="!gridPresets.length">暂无可用预设</p>
      </div>
    </div>

    <button
      type="button"
      class="toolbar-entry"
      :class="{ 'is-blocked': upscale.blocked }"
      :title="titleOf(upscale.name, upscale.blocked)"
      @click.stop="spawnTool('upscale')"
    >
      <span class="toolbar-tag">HD</span><span>{{ upscale.name }}</span>
      <i v-if="upscale.isNew" class="toolbar-badge">NEW</i>
    </button>
    <div class="toolbar-menu">
      <button
        type="button"
        class="toolbar-entry"
        :class="{ active: openMenu === 'gridSplit' }"
        aria-haspopup="menu"
        :aria-expanded="openMenu === 'gridSplit'"
        data-testid="toolbar-grid-split"
        @click.stop="toggle('gridSplit')"
      >
        <Table2 :size="15" /><span>宫格切分</span><ChevronDown :size="12" />
      </button>
      <div v-if="openMenu === 'gridSplit'" class="toolbar-dropdown" role="menu">
        <button
          v-for="entry in gridSplit"
          :key="entry.code"
          type="button"
          role="menuitem"
          :class="{ 'is-blocked': !entry.preset }"
          :title="entry.preset ? entry.label : `${entry.label}：${entry.reason}`"
          @click.stop="choosePreset(entry)"
        >
          <component :is="entry.preset ? presetIcon(entry.preset) : Table2" :size="17" />
          <b>{{ entry.label }}</b>
          <i v-if="entry.preset?.isNew" />
        </button>
      </div>
    </div>

    <span class="toolbar-split" />

    <button type="button" class="toolbar-icon" title="涂抹重绘" @click.stop="placeholder">
      <Wand2 :size="16" />
    </button>
    <button type="button" class="toolbar-icon" title="抠图" @click.stop="placeholder">
      <Scissors :size="16" />
    </button>
    <button type="button" class="toolbar-icon" title="下载" @click.stop="placeholder">
      <Download :size="16" />
    </button>
    <button type="button" class="toolbar-icon" title="放大查看（也可双击图片）" @click.stop="openMenu = ''; emit('preview')">
      <Maximize2 :size="16" />
    </button>
  </div>
</template>

<style scoped>
/* 不可用 / 已下线的入口照样显示（IMG-F-06/08），只是弱一档；原因在悬停提示与下拉里的小字 */
.toolbar-entry.is-blocked,
.toolbar-dropdown button.is-blocked { opacity: .62; }
.toolbar-dropdown button small { margin-left: auto; color: var(--cv-n58); font-size: 11px; white-space: nowrap; }
</style>
