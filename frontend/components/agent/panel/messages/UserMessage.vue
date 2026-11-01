<script setup lang="ts">
/**
 * 用户消息（设计 03 §3.1）：右对齐气泡，上方小字是这一轮的视频模型，引用以胶囊显示、点击在画布上定位。
 * 卡片提交生成的消息，正文由服务端拼成具体内容（「我选择了：方向 1 悲壮离别」），这里原样显示。
 */
import { Copy, Clapperboard } from "@lucide/vue";
import { ElMessage } from "element-plus";
import type { AgentMessage } from "~/utils/agentMessages";
import { AGENT_PANEL_BRIDGE, NOOP_BRIDGE } from "~/utils/agentPanelBridge";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { MESSAGE_IMAGE_LIMIT, userMessageImages } from "~/utils/messageImages";

const props = defineProps<{ message: AgentMessage; videoModel?: string }>();
const bridge = inject(AGENT_PANEL_BRIDGE, NOOP_BRIDGE);
const agent = useAgentStore();

// 消息带的图（商品库商品 / 角色库角色按会话快照换成图，素材直接是图）：气泡里显示缩略图，点开看原图。
// ⚠️ 都是受保护媒体：走 useAuthenticatedMedia 取成 object URL，组件卸载时随作用域释放（CLAUDE.md §2.13）。
const images = computed(() => userMessageImages(props.message.refs, agent.detail?.sceneInput));
const shownImages = computed(() => images.value.slice(0, MESSAGE_IMAGE_LIMIT));
const hiddenImages = computed(() => images.value.length - shownImages.value.length);
const thumbs = useAuthenticatedMedia();
watch(shownImages, list => {
  void thumbs.appendAll(list.map(img => ({ key: img.mediaId, url: mediaThumbUrl(img.mediaId, 160) })));
}, { immediate: true });

// 原图点开才取（一张两三 MB，不预取），关掉预览不释放 —— 来回翻不用重取，卸载时一并释放。
const originals = useAuthenticatedMedia();
const previewAt = ref<number | null>(null);
const previewFailed = ref(false);
const previewing = computed(() => (previewAt.value === null ? undefined : images.value[previewAt.value]));
async function loadPreview() {
  const img = previewing.value;
  if (!img) return;
  previewFailed.value = false;
  await originals.appendAll([{ key: img.mediaId, url: `/media/${img.mediaId}` }]);
  if (previewing.value?.mediaId === img.mediaId && !originals.urls.value[img.mediaId]) previewFailed.value = true;
}
function openPreview(index: number) {
  previewAt.value = index;
  void loadPreview();
}
function stepPreview(delta: number) {
  const count = images.value.length;
  if (previewAt.value === null || count === 0) return;
  previewAt.value = (previewAt.value + delta + count) % count;
  void loadPreview();
}

type Ref = { kind: string; id: string; label: string };
const refs = computed<Ref[]>(() => (Array.isArray(props.message.refs) ? props.message.refs : []).flatMap((raw) => {
  const item = raw && typeof raw === "object" ? raw as Record<string, unknown> : {};
  const id = typeof item.id === "string" ? item.id : "";
  if (!id) return [];
  const kind = typeof item.kind === "string" ? item.kind : "node";
  const label = typeof item.label === "string" && item.label ? item.label : kind === "node" ? "节点" : kind === "asset" ? "资产" : "素材";
  return [{ kind, id, label }];
}));

const time = computed(() => {
  const d = new Date(props.message.createdAt);
  return Number.isNaN(d.getTime()) ? "" : `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
});

function locate(ref: Ref) {
  if (ref.kind !== "node") return;
  if (!bridge.locate([ref.id])) ElMessage.info("这个节点已经不在画布上了");
}

async function copy() {
  try {
    await navigator.clipboard.writeText(props.message.content);
    ElMessage.success("已复制");
  } catch {
    ElMessage.warning("复制没有成功，请手动选择文字");
  }
}
</script>

<template>
  <div class="agent-msg agent-msg-user">
    <small v-if="videoModel" class="agent-msg-model" title="这一轮使用的视频模型"><Clapperboard :size="12" />{{ videoModel }}</small>
    <div class="agent-bubble">
      <div v-if="shownImages.length" class="agent-msg-images">
        <button
          v-for="(img, i) in shownImages"
          :key="img.key"
          type="button"
          class="agent-msg-image"
          :title="img.label"
          :aria-label="`查看图片：${img.label}`"
          @click="openPreview(i)"
        >
          <img v-if="thumbs.urls.value[img.mediaId]" :src="thumbs.urls.value[img.mediaId]" :alt="img.label">
          <span v-else class="agent-msg-image-blank" aria-hidden="true" />
          <span v-if="i === shownImages.length - 1 && hiddenImages > 0" class="agent-msg-image-more">+{{ hiddenImages }}</span>
        </button>
      </div>
      <p>{{ message.content }}</p>
      <div v-if="refs.length" class="agent-ref-chips">
        <button
          v-for="ref in refs"
          :key="`${ref.kind}:${ref.id}`"
          type="button"
          class="agent-chip"
          :class="{ 'is-static': ref.kind !== 'node' }"
          :title="ref.kind === 'node' ? '在画布上定位' : ref.label"
          @click="locate(ref)"
        >
          ▶ {{ ref.label }}
        </button>
      </div>
    </div>
    <footer class="agent-msg-meta"><span>{{ time }}</span><button type="button" @click="copy"><Copy :size="12" />复制</button></footer>
    <MediaPreviewDialog
      v-if="previewing"
      type="image"
      :src="originals.urls.value[previewing.mediaId]"
      :title="previewing.label"
      :loading="!originals.urls.value[previewing.mediaId] && !previewFailed"
      :error="previewFailed ? '这张图取不到了，可能已被删除' : ''"
      :index="previewAt ?? 0"
      :count="images.length"
      @close="previewAt = null"
      @prev="stepPreview(-1)"
      @next="stepPreview(1)"
      @retry="loadPreview"
    />
  </div>
</template>

<style scoped>
/* 图在文字上方，与气泡同宽换行；中性色描边（与输入框统一，marketing dev/03 R-12）。 */
.agent-msg-images { display: flex; flex-wrap: wrap; gap: 6px; margin-bottom: 8px; }
.agent-msg-image {
  position: relative;
  width: 64px;
  height: 64px;
  overflow: hidden;
  border: 1px solid var(--canvas-line, var(--portal-line));
  border-radius: 8px;
  padding: 0;
  background: var(--portal-surface-2);
  cursor: zoom-in;
}
.agent-msg-image:hover { border-color: var(--portal-line-strong); }
.agent-msg-image img { display: block; width: 100%; height: 100%; object-fit: cover; }
.agent-msg-image-blank { display: block; width: 100%; height: 100%; background: var(--portal-surface-3); }
.agent-msg-image-more {
  position: absolute;
  inset: 0;
  display: grid;
  place-items: center;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  font-size: 13px;
  font-weight: 600;
}
</style>
