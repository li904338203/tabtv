<script setup lang="ts">
/**
 * 素材条（SCT-23，SC-F-10~18）：连进智能剪辑的素材，编号（多产物时 3-1~3-4）、缩略图、时长、类型、状态、音频用途。
 * 指令面板与剪辑台共用。数据以服务端 `GET .../inputs` 为准（SCR-11），这里不从画布自己推导。
 *
 * - 还在生成 / 上传的显示「生成中」，归属或类型不合法的显示原因；超出 50 条的不进素材条，只说个数（SC-F-17）；
 * - 剪过之后再连进来的新素材不会自动上轨：显示「有 N 段新素材未使用」与「追加到末尾」（SC-F-14）。
 */
import type { components } from "@tabtv/api-client";
import { smartcutUnusedCount } from "~/utils/smartcutInputs";
import { smartcutMediaUrl } from "~/utils/smartcutPublish";
import { smartcutRoleLabel } from "~/utils/smartcutStudio";
import { smartcutFormatTime } from "~/utils/smartcutTimeline";

type Input = components["schemas"]["SmartcutInput"];

const props = defineProps<{ items: readonly Input[]; omitted?: number; arranged?: boolean; canAppend?: boolean; appendReason?: string }>();
const emit = defineEmits<{ "append-unused": [] }>();
const thumbs = useAuthenticatedMedia();

const thumbOf = (item: Input) => item.posterMediaId ?? (item.kind === "image" ? item.mediaId : undefined);
watch(() => props.items.map(thumbOf).filter(Boolean).join(","), () => {
  const wanted = props.items.map(thumbOf).filter((id): id is string => Boolean(id)).map(id => ({ key: id, url: `${smartcutMediaUrl(id)}?w=160` }));
  if (wanted.length) void thumbs.appendAll(wanted);
}, { immediate: true });

const unused = computed(() => smartcutUnusedCount(props.items));
const KIND_LABELS: Record<string, string> = { video: "视频", image: "图片", audio: "音频", text: "文本", shotGroup: "分镜组" };
function statusText(item: Input): string {
  if (item.status === "pending") return "生成中";
  if (item.status === "failed") return "生成失败";
  if (item.status === "invalid") return item.reason || "不能用";
  return "";
}
</script>

<template>
  <div class="sc-input-strip" data-testid="smartcut-input-strip">
    <p v-if="!items.length" class="sc-input-empty">把视频、图片、音频节点连到这里，素材会自动铺上轨道。</p>
    <ul v-else class="sc-input-list" aria-label="素材条">
      <li v-for="item in items" :key="`${item.no}-${item.sourceNodeId}`" :class="[`status-${item.status}`, { unused: item.status === 'ready' && !item.used }]" :title="item.title || item.text || ''">
        <span class="sc-input-thumb">
          <img v-if="thumbOf(item) && thumbs.urls.value[thumbOf(item)!]" :src="thumbs.urls.value[thumbOf(item)!]" alt="">
          <span v-else>{{ KIND_LABELS[item.kind] ?? item.kind }}</span>
        </span>
        <span class="sc-input-no">#{{ item.no }}</span>
        <span class="sc-input-meta">
          {{ item.durationMs ? smartcutFormatTime(item.durationMs) : KIND_LABELS[item.kind] ?? "" }}
          <template v-if="item.audioRole"> · {{ smartcutRoleLabel(item.audioRole) }}</template>
        </span>
        <span v-if="statusText(item)" class="sc-input-status">{{ statusText(item) }}</span>
      </li>
    </ul>
    <p v-if="omitted" class="sc-input-note">还有 {{ omitted }} 条超出上限（50 条），没进素材条。</p>
    <div v-if="arranged && unused" class="sc-input-unused" role="note">
      <span>有 {{ unused }} 段新素材未使用</span>
      <button type="button" class="sc-input-append nodrag" :disabled="!canAppend" :title="canAppend ? '打开剪辑台，把它们追加到对应轨道的末尾' : appendReason" data-testid="smartcut-append-unused" @click.stop="emit('append-unused')">追加到末尾</button>
    </div>
  </div>
</template>

<style scoped>
.sc-input-strip { display: grid; gap: 6px; }
.sc-input-empty, .sc-input-note { margin: 0; color: var(--canvas-muted, var(--portal-muted)); font-size: 11px; }
.sc-input-list { display: flex; gap: 6px; margin: 0; padding: 0; overflow-x: auto; list-style: none; }
.sc-input-list li { position: relative; flex: 0 0 64px; display: grid; gap: 2px; font-size: 10px; color: var(--canvas-muted, var(--portal-muted)); }
.sc-input-list li.unused .sc-input-thumb { outline: 1px dashed var(--portal-gold); }
.sc-input-thumb { display: grid; place-items: center; width: 64px; height: 40px; overflow: hidden; border-radius: 5px; background: var(--canvas-line, var(--portal-surface-2)); }
.sc-input-thumb img { width: 100%; height: 100%; object-fit: cover; }
.sc-input-no { position: absolute; top: 2px; left: 3px; padding: 0 3px; border-radius: 3px; color: var(--portal-on-media); background: var(--portal-on-media-bg); }
.sc-input-status { color: var(--portal-gold); }
.status-failed .sc-input-status, .status-invalid .sc-input-status { color: var(--portal-danger); }
.sc-input-unused { display: flex; align-items: center; justify-content: space-between; gap: 6px; font-size: 11px; color: var(--portal-gold); }
.sc-input-append { height: 22px; padding: 0 8px; border: 1px solid var(--portal-gold-line); border-radius: 5px; color: var(--portal-gold); background: transparent; font-size: 11px; cursor: pointer; }
.sc-input-append:disabled { opacity: 0.5; cursor: not-allowed; }
</style>
