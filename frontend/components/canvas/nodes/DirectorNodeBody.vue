<script setup lang="ts">
/**
 * 3D 导演台的节点卡（DRT-16，dev/03 §6.1）：封面（输出机位的最新截图）或空场景占位、截图数、「打开导演台」。
 *
 * ⚠️ 卡片**不开 WebGL**（DR-F-05、DRK-15）：同一页面同时只有一个活的 3D 渲染器，就是打开着的那个编辑器。
 *    画布上摆十个导演台节点就开十个上下文的话，浏览器会开始丢别的上下文（含画布本身）。
 * ⚠️ 封面是受保护媒体：`<img src>` 不带鉴权头，直接引用一律 401 —— 经 useAuthenticatedMedia 取成 blob，
 *    卸载时释放；不依赖只在页面挂载时跑一次的 generation.restore()（DRB-7）。
 * 打开只发 `open` 事件，由 CanvasBoard 先做自愈判定（DRD-43）再挂载编辑器 —— 这里不碰场景接口。
 * 过期角标（DRT-17，DR-F-83）：输出机位那条截图的指纹 ≠ 场景当前的指纹；「重新截图」打开编辑器并截输出机位。
 */
import { Clapperboard, RefreshCw } from "@lucide/vue";
import type { JsonObject } from "~/types/canvas";
import { directorCaptureCount, directorCoverUrl, directorSceneIdOf } from "~/utils/directorCanvas";
import { directorNodeStale } from "~/utils/directorStale";

const props = defineProps<{ params: JsonObject; title: string }>();
const emit = defineEmits<{ open: []; recapture: [] }>();

const scenes = useDirectorScenes();
const stale = computed(() => directorNodeStale(props.params, scenes.summaries.value[directorSceneIdOf(props.params)]));

const covers = useAuthenticatedMedia();
const coverUrl = computed(() => directorCoverUrl(props.params));
const coverSrc = computed(() => covers.urls.value.cover ?? "");
const captureCount = computed(() => directorCaptureCount(props.params));

watch(coverUrl, (url) => {
  covers.release();
  if (url) void covers.appendAll([{ key: "cover", url }]);
}, { immediate: true });
</script>

<template>
  <!-- ⚠️ `.stop`：双击冒泡到画布的话 Vue Flow 会顺手把视口放大一级 -->
  <div class="director-node-body" title="双击打开导演台" @dblclick.stop="emit('open')">
    <div class="director-node-cover">
      <img v-if="coverSrc" :src="coverSrc" :alt="`${title} 的封面`" :draggable="false">
      <svg v-else class="director-node-placeholder" viewBox="0 0 320 180" role="img" aria-label="空场景">
        <!-- 地面网格：透视线汇向画面中上方，横线越远越密 -->
        <g class="grid">
          <path d="M160 92 L-40 180 M160 92 L40 180 M160 92 L120 180 M160 92 L200 180 M160 92 L280 180 M160 92 L360 180" />
          <path d="M18 154 H302 M52 126 H268 M80 106 H240 M100 96 H220" />
        </g>
        <!-- 一个素模剪影 -->
        <g class="figure">
          <ellipse cx="160" cy="146" rx="24" ry="5" class="shadow" />
          <circle cx="160" cy="64" r="9" />
          <path d="M149 76 Q160 72 171 76 L175 106 Q170 108 168 106 L166 92 L165 112 L168 144 L161 144 L160 116 L159 144 L152 144 L155 112 L154 92 L152 106 Q150 108 145 106 Z" />
        </g>
      </svg>
      <button type="button" class="director-node-open nodrag" @mousedown.stop @click.stop="emit('open')">
        <Clapperboard :size="15" />打开导演台
      </button>
    </div>
    <footer class="director-node-meta">
      <span>{{ captureCount ? `截图 ${captureCount}` : '还没有截图' }}</span>
      <template v-if="stale">
        <span class="director-node-stale" title="场景已更新，输出机位的截图可能过期">截图已过期</span>
        <button type="button" class="director-node-recapture nodrag" title="打开导演台，重新截输出机位" @mousedown.stop @click.stop="emit('recapture')">
          <RefreshCw :size="11" />重新截图
        </button>
      </template>
    </footer>
  </div>
</template>

<style scoped>
.director-node-body { display: grid; grid-template-rows: auto 1fr; min-height: 212px; border-radius: 7px; overflow: hidden; }
.director-node-cover { position: relative; aspect-ratio: 16 / 9; overflow: hidden; background: var(--cv-n12); cursor: move; }
.director-node-cover img { width: 100%; height: 100%; display: block; object-fit: cover; pointer-events: none; user-select: none; }
.director-node-placeholder { width: 100%; height: 100%; display: block; color: var(--cv-n50); }
.director-node-placeholder .grid path { fill: none; stroke: currentColor; stroke-width: 1; opacity: 0.45; }
.director-node-placeholder .figure { fill: currentColor; opacity: 0.75; }
.director-node-placeholder .figure .shadow { opacity: 0.35; }
.director-node-open {
  position: absolute;
  left: 50%;
  top: 50%;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 7px 14px;
  border: 1px solid var(--cv-tint-22);
  border-radius: 999px;
  color: var(--cv-n96);
  background: var(--cv-n09a84);
  font-size: 12px;
  opacity: 0;
  transform: translate(-50%, -50%);
  transition: opacity 120ms ease;
  cursor: pointer;
}
/* 悬停出现（DR-UX-14）；选中时也显示，键盘聚焦时同样可见 */
:global(.aitv-node:hover) .director-node-open,
:global(.aitv-node.selected) .director-node-open,
.director-node-open:focus-visible { opacity: 1; }
.director-node-open:hover { background: var(--cv-n21); }
.director-node-meta { display: flex; align-items: center; gap: 8px; padding: 0 12px; color: var(--cv-n63); font-size: 11px; }
.director-node-stale { padding: 1px 6px; border-radius: 999px; color: var(--cv-n96); background: var(--cv-n21); }
.director-node-recapture { display: inline-flex; align-items: center; gap: 3px; margin-left: auto; padding: 2px 8px; border: 1px solid var(--cv-tint-22); border-radius: 999px; color: var(--cv-n90); background: transparent; font-size: 11px; cursor: pointer; }
.director-node-recapture:hover { background: var(--cv-n21); }
</style>
