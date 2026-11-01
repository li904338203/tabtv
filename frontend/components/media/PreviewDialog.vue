<script setup lang="ts">
/**
 * 媒体预览浮层 —— 首页 / AI 视频页的「灵感与作品」与「创作资产」**共用这一个**。
 *
 * ⚠️ 此前首页与 AI 视频页各抄了一份一模一样的标记，创作资产要预览时就会有第三份；
 *    抄三份的话，哪天调一处（比如关闭方式、视频的标签）另两处不会跟着变。
 *    外观（2026-10-30 起）是「透明」的：只有画面浮在半透明遮罩上，没有白底框与底部文字，样式在本组件里（`.preview-lightbox`）。
 *
 * 只负责展示与关闭，**不负责取字节**：公开精选给的是公开地址，创作资产给的是
 * fetch 成 blob 之后的 object URL —— 两种来源由调用方决定，这里只认一个 `src`。
 */
import { ChevronLeft, ChevronRight, LoaderCircle, X } from "@lucide/vue";

const props = defineProps<{
  type?: string;
  /** 能直接交给 <img>/<video>/<audio> 的地址（公开地址或 object URL）。 */
  src?: string;
  /** 没有 src 时的兜底图（精选作品的封面）。 */
  poster?: string;
  title?: string;
  subtitle?: string;
  loading?: boolean;
  error?: string;
  /** 多产物时的位置（从 0 起）与总数；总数 ≤ 1 时不出现切换按钮。 */
  index?: number;
  count?: number;
}>();
const emit = defineEmits<{ close: []; prev: []; next: []; retry: [] }>();

const multiple = computed(() => (props.count ?? 0) > 1);

/**
 * ⚠️ 浮层打开时**独占键盘**：在捕获阶段先拿到按键并停止传播。
 *    画布在 window 上挂着快捷键 —— 选中节点时 Backspace/Delete 删除它、Ctrl+Z 撤销、Ctrl+D 复制。
 *    从画布上双击打开预览时那个节点必然是选中的：不拦的话在预览里按一下 Delete，
 *    被浮层挡住的节点就没了，而用户完全看不到发生了什么。
 *    停止传播不影响默认行为（Tab 切焦点、回车/空格触发按钮照常）。
 */
function onKeydown(event: KeyboardEvent) {
  event.stopPropagation();
  if (event.key === "Escape") emit("close");
  else if (multiple.value && event.key === "ArrowLeft") emit("prev");
  else if (multiple.value && event.key === "ArrowRight") emit("next");
}
onMounted(() => window.addEventListener("keydown", onKeydown, true));
onBeforeUnmount(() => window.removeEventListener("keydown", onKeydown, true));
</script>

<template>
  <!-- 浮层是 position: fixed；但祖先带 transform 时（画布节点里）会以那个祖先为参照，调用方要 Teleport 到 body -->
  <!-- 「透明」样式（2026-10-30 用户反馈）：只有画面浮在半透明遮罩上，不再有白底框和底部的标题 / 类型字样；
       标题只留给读屏（aria-label），操作（下载）是右上角的半透明按钮 -->
  <div class="portal-overlay player-overlay preview-lightbox" @click.self="emit('close')">
    <section class="preview-dialog" role="dialog" aria-modal="true" :aria-label="title || '预览'" @click.self="emit('close')">
      <div class="preview-dialog-toolbar">
        <div class="preview-dialog-actions">
          <slot name="actions" />
        </div>
        <button type="button" class="preview-dialog-close" title="关闭（Esc）" aria-label="关闭" @click="emit('close')"><X :size="18" /></button>
      </div>

      <div v-if="loading" class="preview-dialog-state"><LoaderCircle :size="22" class="spin" />正在加载预览</div>
      <div v-else-if="error" class="preview-dialog-state">
        {{ error }}
        <button type="button" class="portal-link-button" @click="emit('retry')">重试</button>
      </div>
      <!-- 视频与音频要用对应标签：塞进 <img> 只会得到一个裂图，点开也听不到声 -->
      <video v-else-if="type === 'video' && src" :src="src" controls autoplay playsinline />
      <audio v-else-if="type === 'audio' && src" :src="src" controls />
      <img v-else-if="src || poster" :src="src || poster" :alt="title">

      <!-- 一次生成出了好几项（GO-40）：逐项看、逐项下，而不是只给第一项 -->
      <template v-if="multiple && !loading">
        <button type="button" class="preview-dialog-nav prev" title="上一项（←）" aria-label="上一项" @click="emit('prev')"><ChevronLeft :size="20" /></button>
        <button type="button" class="preview-dialog-nav next" title="下一项（→）" aria-label="下一项" @click="emit('next')"><ChevronRight :size="20" /></button>
        <span class="preview-dialog-count">{{ (index ?? 0) + 1 }} / {{ count }}</span>
      </template>
    </section>
  </div>
</template>

<style scoped>
/* 半透明遮罩 + 模糊；画面本身浮在上面，没有白底框 */
.preview-lightbox { background: rgba(8, 9, 11, .72); backdrop-filter: blur(10px); -webkit-backdrop-filter: blur(10px); }
.preview-lightbox .preview-dialog {
  position: relative; width: auto; max-width: min(92vw, 1400px); display: grid; place-items: center; overflow: visible;
  border: 0; border-radius: 0; background: transparent; box-shadow: none;
}
.preview-lightbox .preview-dialog > img,
.preview-lightbox .preview-dialog > video {
  display: block; width: auto; max-width: min(92vw, 1400px); max-height: 86vh; object-fit: contain;
  border-radius: 10px; background: transparent; box-shadow: 0 24px 60px rgba(0, 0, 0, .45);
}
.preview-lightbox .preview-dialog > audio { width: min(520px, 86vw); padding: 0; background: transparent; }
.preview-lightbox .preview-dialog-state {
  min-height: 0; min-width: 240px; padding: 40px 32px; border-radius: 10px; color: rgba(255, 255, 255, .78); background: rgba(255, 255, 255, .06);
}
/* 右上角：操作 + 关闭，半透明圆角按钮，贴在视口右上而不是画面上（竖屏视频两侧留白时也找得到） */
.preview-lightbox .preview-dialog-toolbar { position: fixed; top: 18px; right: 18px; z-index: 3; display: flex; align-items: center; gap: 8px; }
.preview-lightbox .preview-dialog-actions { display: flex; align-items: center; gap: 8px; }
/* 调用方塞进来的类型角标（VIDEO / IMAGE）不再显示：用户不要描述文字 */
.preview-lightbox .preview-dialog-actions :deep(.media-kind) { display: none; }
.preview-lightbox .preview-dialog-actions :deep(button),
.preview-lightbox .preview-dialog-close {
  position: static; height: 36px; min-width: 36px; display: inline-flex; align-items: center; justify-content: center; gap: 6px; padding: 0 14px;
  border: 1px solid rgba(255, 255, 255, .18); border-radius: 999px; color: #fff; background: rgba(255, 255, 255, .12);
  font-size: 13px; cursor: pointer; transition: background 120ms ease;
}
.preview-lightbox .preview-dialog-close { width: 36px; padding: 0; }
.preview-lightbox .preview-dialog-actions :deep(button:hover:not(:disabled)),
.preview-lightbox .preview-dialog-close:hover { background: rgba(255, 255, 255, .22); }
.preview-lightbox .preview-dialog-actions :deep(button:disabled) { opacity: .45; cursor: not-allowed; }
.preview-lightbox .preview-dialog > .preview-dialog-nav {
  position: fixed; top: 50%; transform: translateY(-50%); width: 44px; height: 44px; display: grid; place-items: center;
  border: 1px solid rgba(255, 255, 255, .18); border-radius: 50%; color: #fff; background: rgba(255, 255, 255, .12); cursor: pointer;
}
.preview-lightbox .preview-dialog > .preview-dialog-nav:hover { background: rgba(255, 255, 255, .22); }
.preview-lightbox .preview-dialog > .preview-dialog-nav.prev { left: 24px; right: auto; }
.preview-lightbox .preview-dialog > .preview-dialog-nav.next { right: 24px; left: auto; }
.preview-lightbox .preview-dialog-count {
  position: fixed; left: 50%; bottom: 22px; transform: translateX(-50%); padding: 4px 12px; border-radius: 999px;
  color: #fff; background: rgba(255, 255, 255, .12); font-size: 12px; font-variant-numeric: tabular-nums;
}
</style>
