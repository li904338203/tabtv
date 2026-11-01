<script setup lang="ts">
/**
 * 确认卡「更换」用的商品图选择器（MKT-T-24，营销 dev/03 §3.2；MKT-F-48 ②、MKT-AC-26 ⑤）。
 *
 * 从会话的商品图（`sceneInput.products`，[0] 为主图）里选 ≤ 上限张：点一张追加到末尾、再点取消，
 * 角标是**选的先后**（即发给模型的参考图顺序），图下是它在会话里的序号。确定时交回序号（从 1 起），
 * 由卡片组进 `itemOverrides` 随批准一起提交 —— 这里不发任何请求、不估价（金额由服务端重报价，MK-10）。
 *
 * ⚠️ 到上限时**不挤掉**已选的那张（用户不会注意到哪张被挤掉了），说清「先取消一张」。
 * ⚠️ 商品图是受保护媒体：缩略图走 `useAuthenticatedMedia`（取 160 档），卸载时随作用域释放（必须 revoke，CLAUDE.md §2.13）。
 */
import { Check } from "@lucide/vue";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { planSelectionError, togglePlanSelection } from "~/utils/planItems";
import { marketingProductTitle } from "~/utils/marketingLibrary";

const props = defineProps<{
  /** 会话商品图的媒体 ID（有序）。 */
  products: string[];
  /** 当前选中的序号（从 1 起，按发送顺序）。 */
  selected: number[];
  /** 本项最多几张（`imageLimit`）。 */
  limit: number;
  /** 会话快照里的商品名（`sceneInput.product.name`，MKT-F-99）；旧会话可能为空。 */
  productName?: string;
}>();
const emit = defineEmits<{ confirm: [indexes: number[]]; cancel: [] }>();

const thumbs = useAuthenticatedMedia();
const picked = ref<number[]>([...props.selected]);
const hint = ref("");

watch(() => props.products, ids => {
  void thumbs.appendAll(ids.map(id => ({ key: id, url: mediaThumbUrl(id, 160) })));
}, { immediate: true });

const error = computed(() => planSelectionError(picked.value, props.limit, props.products.length));

function toggle(index: number) {
  const result = togglePlanSelection(picked.value, index, props.limit);
  picked.value = result.indexes;
  hint.value = result.reason;
}

function confirm() {
  if (error.value) return;
  emit("confirm", [...picked.value]);
}
</script>

<template>
  <div class="product-picker" role="group" aria-label="选择本项使用的商品图">
    <p class="product-picker-head">
      <span>选择本项使用的商品图<template v-if="marketingProductTitle(productName)"> · {{ marketingProductTitle(productName) }}</template></span>
      <small>已选 {{ picked.length }} / {{ limit }}</small>
    </p>
    <ul class="product-picker-grid">
      <li v-for="(id, i) in products" :key="`${id}-${i}`">
        <button
          type="button"
          class="product-picker-item"
          :class="{ on: picked.includes(i + 1) }"
          :aria-pressed="picked.includes(i + 1)"
          :title="`第 ${i + 1} 张${i === 0 ? '（主图）' : ''}`"
          @click="toggle(i + 1)"
        >
          <img v-if="thumbs.urls.value[id]" :src="thumbs.urls.value[id]" :alt="`商品图 ${i + 1}`">
          <span v-else class="product-picker-blank" aria-hidden="true" />
          <span v-if="picked.includes(i + 1)" class="product-picker-order">{{ picked.indexOf(i + 1) + 1 }}</span>
          <Check v-if="picked.includes(i + 1)" :size="12" class="product-picker-check" />
          <small class="product-picker-no">#{{ i + 1 }}</small>
        </button>
      </li>
    </ul>
    <p v-if="hint || error" class="product-picker-error" role="alert">{{ hint || error }}</p>
    <p class="product-picker-note">角标是发给模型的顺序；换图后费用以确认时的报价为准</p>
    <div class="product-picker-actions">
      <button type="button" class="agent-secondary" @click="emit('cancel')">取消</button>
      <button type="button" class="agent-primary" :disabled="Boolean(error)" @click="confirm">确定</button>
    </div>
  </div>
</template>

<style scoped>
.product-picker { display: grid; gap: 8px; margin-top: 6px; padding: 10px; border: 1px solid var(--canvas-line, var(--portal-line)); border-radius: 10px; }
.product-picker-head { margin: 0; display: flex; justify-content: space-between; align-items: baseline; gap: 8px; font-size: 12px; }
.product-picker-head small { color: var(--canvas-muted, var(--portal-muted)); }
.product-picker-grid { margin: 0; padding: 0; list-style: none; display: grid; grid-template-columns: repeat(auto-fill, minmax(56px, 1fr)); gap: 6px; }
.product-picker-item {
  position: relative;
  width: 100%;
  aspect-ratio: 1;
  padding: 0;
  overflow: hidden;
  border: 1px solid var(--canvas-line, var(--portal-line));
  border-radius: 8px;
  background: var(--canvas-surface, var(--portal-surface-2));
  cursor: pointer;
}
.product-picker-item img, .product-picker-blank { display: block; width: 100%; height: 100%; object-fit: cover; }
/* 中性色（它嵌在面板的计划确认卡里，与面板统一，design/03 R-3）。 */
.product-picker-item.on { border-color: var(--canvas-text, var(--portal-text)); box-shadow: inset 0 0 0 1px var(--canvas-text, var(--portal-text)); }
.product-picker-order {
  position: absolute;
  top: 3px;
  left: 3px;
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  border-radius: 999px;
  color: var(--cv-n06, var(--portal-mark-text));
  background: var(--cv-n94, var(--portal-mark-bg));
  font-size: 10px;
  line-height: 16px;
  text-align: center;
}
/* 叠在图片上：用媒体控件的白色 + 阴影，任何底图上都看得清。 */
.product-picker-check { position: absolute; top: 4px; right: 4px; color: var(--portal-on-media); filter: drop-shadow(0 0 2px var(--portal-shadow-strong)); }
.product-picker-no {
  position: absolute;
  right: 3px;
  bottom: 2px;
  padding: 0 3px;
  border-radius: 4px;
  color: var(--canvas-text, var(--portal-text));
  background: var(--canvas-panel, var(--portal-surface));
  font-size: 10px;
  opacity: .85;
}
.product-picker-error { margin: 0; color: var(--canvas-danger, var(--portal-danger)); font-size: 12px; }
.product-picker-note { margin: 0; color: var(--canvas-muted, var(--portal-muted)); font-size: 11px; }
.product-picker-actions { display: flex; justify-content: flex-end; gap: 6px; }
</style>
