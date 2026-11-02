<script setup lang="ts">
/**
 * 模板墙的一张卡（MKT-T-15，营销 dev/03 §5；MKT-F-31/32）：三联竖版封面 + 标题 + 一行说明。
 *
 * - 封面：`coverCount` 张，不足三张按实际张数等分（`utils/mediaGrid.coverPanels`）；
 *   封面行的高宽比固定按「三格 9:16」算，张数少的卡不会比别的卡更高（网格对不齐）。
 *   每格里的图**按原比例完整显示**、同一张图放大虚化垫底（营销 dev/03 R-13）：此前每格 cover 裁切，
 *   而实际每个选项只有 1 张 9:16 封面，铺满 27:16 的整行后只露出中间约三分之一，人物的头被裁掉。
 *   取图走公开端点 `/marketing/options/{code}/covers/{i}?w=320`，直接 `<img>`（未登录也看得到，MKT-F-03）。
 * - 悬停：`hasPreview` 且设备能精确悬停（`hoverPlay`，由墙算一次传进来）时，示例视频静音盖在封面上播放、离开即停。
 *   ⚠️ 视频 `preload="none"`：一屏 24 张卡各预取一段视频，进页就是几十 MB 的流量，而大多数卡从不会被悬停。
 * - 点击只抛出 `pick`：填入由墙交给输入框，**卡片自己不发送、不建会话**（MK-16）。
 */
import { ImageOff } from "@lucide/vue";
import { optionCoverUrl, optionPreviewUrl, type MarketingOption } from "~/utils/marketingOptions";
import { coverPanels, startHoverPreview, stopHoverPreview } from "~/utils/mediaGrid";

const props = defineProps<{
  option: MarketingOption;
  apiBase: string;
  /** 本设备是否启用悬停播放（`canHoverPlay`）。 */
  hoverPlay: boolean;
}>();

const emit = defineEmits<{ pick: [option: MarketingOption] }>();

const panels = computed(() => coverPanels(props.option.coverCount));
/** 取图失败的格：回落占位，而不是裂图。 */
const broken = ref(new Set<number>());
const video = ref<HTMLVideoElement>();
const playing = ref(false);

const withPreview = computed(() => props.hoverPlay && props.option.hasPreview);
// Linux 桌面上直接地址播不了，换成 blob（TT-T-16）；不用 PlayableVideo 组件是因为这里要拿 <video> 元素 ref。
const previewSrc = usePlayableSrc(() => (withPreview.value ? optionPreviewUrl(props.apiBase, props.option.code) : null));

watch(() => props.option.code, () => {
  broken.value = new Set();
});

function markBroken(index: number) {
  broken.value = new Set(broken.value).add(index);
}

function enter() {
  if (!withPreview.value || !video.value) return;
  playing.value = true;
  startHoverPreview(video.value);
}

function leave() {
  if (!video.value) return;
  playing.value = false;
  stopHoverPreview(video.value, 0);
}
</script>

<template>
  <button
    type="button"
    class="tpl-card"
    :title="`填入创意：${option.name}`"
    @mouseenter="enter"
    @mouseleave="leave"
    @click="emit('pick', option)"
  >
    <span class="tpl-covers">
      <template v-if="panels.indexes.length">
        <span v-for="i in panels.indexes" :key="i" class="tpl-panel">
          <span v-if="broken.has(i)" class="tpl-placeholder"><ImageOff :size="18" /></span>
          <template v-else>
            <!-- 垫底：同一张图（同一个地址，浏览器只取一次）放大虚化铺满格子；前景完整显示、不裁切 -->
            <img class="tpl-panel-bg" :src="optionCoverUrl(apiBase, option.code, i, 320)" alt="" aria-hidden="true" loading="lazy">
            <img
              class="tpl-panel-img"
              :src="optionCoverUrl(apiBase, option.code, i, 320)"
              :alt="`${option.name} 封面 ${i + 1}`"
              loading="lazy"
              @error="markBroken(i)"
            >
          </template>
        </span>
      </template>
      <span v-else class="tpl-panel"><span class="tpl-placeholder"><ImageOff :size="18" /></span></span>
      <video
        v-if="withPreview"
        ref="video"
        class="tpl-preview"
        :class="{ on: playing }"
        :src="previewSrc"
        muted
        loop
        playsinline
        preload="none"
        aria-hidden="true"
      />
    </span>
    <span class="tpl-name">{{ option.name }}</span>
    <span class="tpl-desc">{{ option.description }}</span>
  </button>
</template>

<style scoped>
.tpl-card {
  display: flex;
  flex-direction: column;
  min-width: 0;
  width: 100%;
  border: 1px solid var(--portal-line);
  border-radius: 14px;
  padding: 8px 8px 14px;
  color: var(--portal-text);
  background: var(--portal-surface);
  text-align: left;
  cursor: pointer;
  transition: border-color 160ms ease, background 160ms ease;
}
.tpl-card:hover { border-color: var(--portal-line-strong); background: var(--portal-hover); }
.tpl-card:focus-visible { outline: 2px solid var(--portal-cyan); outline-offset: 2px; }

/* 三格 9:16 并排 → 整行 27:16；张数少时等分同一行宽。每格图片完整显示（contain），同图虚化垫底（营销 dev/03 R-13） */
.tpl-covers { position: relative; display: flex; gap: 3px; width: 100%; aspect-ratio: 27 / 16; overflow: hidden; border-radius: 9px; }
.tpl-panel { position: relative; flex: 1 1 0; min-width: 0; height: 100%; overflow: hidden; border-radius: 6px; background: var(--portal-surface-3); }
.tpl-panel-bg { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; filter: blur(16px) brightness(.85); transform: scale(1.15); }
.tpl-panel-img { position: relative; display: block; width: 100%; height: 100%; object-fit: contain; }
.tpl-placeholder { display: grid; place-items: center; width: 100%; height: 100%; color: var(--portal-faint); }
/* 亮色主题：白底封面在浅色卡片上会「消失」，加 1px 描边（营销 dev/03 §9） */
:root[data-theme="light"] .tpl-panel { outline: 1px solid var(--portal-line); outline-offset: -1px; }

/* 预览视频同样完整显示（contain）；透明底：两侧露出下面的虚化封面，而不是黑边 */
.tpl-preview { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: contain; border-radius: 9px; background: transparent; opacity: 0; pointer-events: none; transition: opacity 160ms ease; }
.tpl-preview.on { opacity: 1; }

.tpl-name { margin: 14px 10px 0; overflow: hidden; font-size: 14px; font-weight: 650; text-overflow: ellipsis; white-space: nowrap; }
.tpl-desc { min-height: 1.5em; margin: 6px 10px 0; overflow: hidden; color: var(--portal-muted); font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }
</style>
