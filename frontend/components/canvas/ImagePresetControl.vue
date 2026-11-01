<script setup lang="ts">
import type { components } from '~/types/legacy';
import {
  Aperture, Box, ChevronDown, Clapperboard, Clock, Film, Globe, Grid2x2, Grid3x3, Images,
  Layers, LayoutGrid, LibraryBig, Package, PersonStanding, ScanFace, Sparkles, SquareUserRound,
  UserRound, UsersRound
} from "@lucide/vue";
import { isPickerPreset, presetCodeOf, presetGroupOf } from "~/utils/canvas";

type PresetItem = components["schemas"]["PresetResp"];
const props = defineProps<{ presets: PresetItem[]; selected: string }>();
const emit = defineEmits<{ select: [value: PresetItem] }>();
const root = ref<HTMLElement>();
const open = ref(false);

/**
 * 分组顺序按截图定版：分镜叙事与质感调节在左列，空间与机位与设定图在右列。
 * 顺序写死在前端而不是按 sort 推导 —— sort 只保证组内次序，组间的左右分布是版式决定的。
 */
const GROUP_NAMES: Record<string, string> = {
  storyboard: "分镜叙事",
  tuning: "质感调节",
  space: "空间与机位",
  sheet: "设定图"
};
const LEFT_GROUPS = ["storyboard", "tuning"];
const RIGHT_GROUPS = ["space", "sheet"];

/** 图标名来自 image_preset.icon，与 lucide 的 kebab-case 命名对齐；认不出的退回分组默认图标。 */
const ICONS: Record<string, unknown> = {
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
  box: Box,
  clapperboard: Clapperboard,
  "users-round": UsersRound,
  "layout-grid": LayoutGrid
};
const GROUP_ICONS: Record<string, unknown> = {
  sheet: UsersRound,
  tuning: Sparkles,
  storyboard: LayoutGrid,
  space: Globe
};

export type { PresetItem };

// ⚠️ 预设码与分组的字段名两边不同（老 `id`/`group`，新 **`code`/`presetGroup`**），
//    归一走 utils 里那一份 —— 在这里再判一次就又多一处会漂移的抄本。
//    读错的表现：每一项的 key 都是 `"undefined"`（互相顶掉），分组全落进"其他"。
function code(item: PresetItem) {
  return presetCodeOf(item);
}
function icon(item: PresetItem) {
  return ICONS[item.icon ?? ""] ?? GROUP_ICONS[presetGroupOf(item)] ?? Images;
}
/** 只列「玩法」：`tool` 组是工具条上的加工工具，不进这个弹窗（判据见 isPickerPreset）。 */
const pickerPresets = computed(() => props.presets.filter(isPickerPreset));
function groupsFor(keys: string[]) {
  return keys
    .map(key => ({
      key,
      name: GROUP_NAMES[key] ?? key,
      items: pickerPresets.value.filter(item => presetGroupOf(item) === key)
    }))
    .filter(group => group.items.length > 0);
}
const leftColumn = computed(() => groupsFor(LEFT_GROUPS));
const rightColumn = computed(() => groupsFor(RIGHT_GROUPS));
// 分组之外的历史预设不能吞掉：停用要靠后台，不该由前端的分组白名单悄悄隐藏
const ungrouped = computed(() => pickerPresets.value.filter(
  item => ![...LEFT_GROUPS, ...RIGHT_GROUPS].includes(presetGroupOf(item))));
const hasAny = computed(() => leftColumn.value.length || rightColumn.value.length || ungrouped.value.length);
const selectedName = computed(() =>
  props.presets.find(item => code(item) === props.selected)?.name ?? "图片预设");

function choose(item: PresetItem) {
  emit("select", item);
  open.value = false;
}
function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) open.value = false;
}
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
</script>

<template>
  <div ref="root" class="canvas-control control-dialog image-preset-control">
    <button type="button" :title="selectedName" @click.stop="open = !open">
      <LayoutGrid :size="14" /><span>{{ selectedName }}</span><ChevronDown :size="13" />
    </button>
    <div v-if="open" class="image-preset-popover nodrag nowheel">
      <div v-if="hasAny" class="image-preset-columns">
        <div class="image-preset-column">
          <section v-for="group in leftColumn" :key="group.key">
            <strong>{{ group.name }}</strong>
            <button
              v-for="item in group.items"
              :key="code(item)"
              type="button"
              :class="{ active: code(item) === selected }"
              @click="choose(item)"
            >
              <span><component :is="icon(item)" :size="18" /><i v-if="item.isNew" /></span>
              <b>{{ item.name }}</b>
            </button>
          </section>
        </div>
        <div class="image-preset-column">
          <section v-for="group in rightColumn" :key="group.key">
            <strong>{{ group.name }}</strong>
            <button
              v-for="item in group.items"
              :key="code(item)"
              type="button"
              :class="{ active: code(item) === selected }"
              @click="choose(item)"
            >
              <span><component :is="icon(item)" :size="18" /><i v-if="item.isNew" /></span>
              <b>{{ item.name }}</b>
            </button>
          </section>
          <section v-if="ungrouped.length">
            <strong>其他</strong>
            <button
              v-for="item in ungrouped"
              :key="code(item)"
              type="button"
              :class="{ active: code(item) === selected }"
              @click="choose(item)"
            >
              <span><component :is="icon(item)" :size="18" /><i v-if="item.isNew" /></span>
              <b>{{ item.name }}</b>
            </button>
          </section>
        </div>
      </div>
      <p v-else>暂无可用预设</p>
    </div>
  </div>
</template>
