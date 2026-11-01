<script setup lang="ts">
/**
 * 发送键左边的价格（设计 02 §4.6）：精确报价 `✦60`，点开看算式；有折扣时原价划线。
 * 文案全部来自 `utils/composerPrice.priceView`（纯函数，有测试），这里只负责摆放。
 */
import type { AgentPriceView, PriceView } from "~/utils/composerPrice";
import { splitCreditLabel } from "~/utils/composerUi";

/**
 * Agent 模式（AGT-29）传 `agent`：显示每轮单价 `✦2/轮`，明细逐行（对话按轮、所选视频 / 图片单价）
 * 并固定带一句「生成费用在确认规划时给出总价，确认前不会扣费」。
 */
defineProps<{ view?: PriceView; agent?: AgentPriceView; /** 浮层向上弹（输入框贴底时） */ up?: boolean }>();
const root = ref<HTMLElement>();
const open = ref(false);

function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) open.value = false;
}
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
</script>

<template>
  <div v-if="agent" ref="root" class="price-pill" :class="{ up }">
    <button type="button" :class="{ failed: agent.error }" :title="agent.error || '对话按轮计费'" :aria-expanded="open" @click.stop="open = !open">
      <span v-if="splitCreditLabel(agent.label).star" class="price-star" aria-hidden="true">✦</span>
      <span>{{ splitCreditLabel(agent.label).text }}</span>
    </button>
    <div v-if="open" class="price-popover" role="dialog" aria-label="费用明细">
      <strong>费用说明</strong>
      <p v-if="agent.error" class="price-error">{{ agent.error }}</p>
      <p v-for="line in agent.lines" :key="line">{{ line }}</p>
      <small>{{ agent.note }}</small>
    </div>
  </div>
  <div v-else-if="view" ref="root" class="price-pill" :class="{ up }">
    <button type="button" :class="{ failed: view.error }" :title="view.error || view.formula || '预计消耗'" :aria-expanded="open" @click.stop="open = !open">
      <span v-if="splitCreditLabel(view.label).star" class="price-star" aria-hidden="true">✦</span>
      <s v-if="view.original">{{ view.original }}</s>
      <span>{{ splitCreditLabel(view.label).text }}</span>
    </button>
    <div v-if="open" class="price-popover" role="dialog" aria-label="费用明细">
      <template v-if="view.error">
        <strong>暂时无法生成</strong>
        <p class="price-error">{{ view.error }}</p>
      </template>
      <template v-else>
        <strong>本次预计消耗</strong>
        <p>{{ view.formula || '还没有报价' }}</p>
        <p v-if="view.original">原价 <s>{{ view.original }}</s><template v-if="view.discountReason">（{{ view.discountReason }}）</template></p>
        <small>价格由服务端按当前定价给出，生成受理时扣费；失败会按原批次退回。</small>
      </template>
    </div>
  </div>
</template>

<style scoped>
/* 工具栏右组的价格：无边框的弱文字，✦ 着品牌色；点开看明细（设计 02 §4.6） */
.price-pill { position: relative; }
.price-pill > button {
  height: 30px;
  display: inline-flex;
  align-items: center;
  gap: 3px;
  border: 0;
  border-radius: 999px;
  padding: 0 8px;
  color: var(--portal-text-soft);
  background: transparent;
  font: inherit;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
  white-space: nowrap;
}
.price-pill > button:hover { color: var(--portal-text); background: var(--portal-hover); }
.price-pill > button.failed { color: var(--portal-muted); font-weight: 400; }
.price-star { color: var(--portal-text-soft); font-size: 11px; }
.price-pill > button.failed .price-star { color: var(--portal-muted); }
.price-pill s { color: var(--portal-faint); font-weight: 400; }
.price-popover {
  width: 260px;
  max-width: calc(100vw - 24px);
  position: absolute;
  top: 38px;
  right: 0;
  z-index: 80;
  display: grid;
  gap: 6px;
  padding: 12px 14px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 12px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 18px 48px var(--portal-shadow-strong);
  font-size: 12px;
}
.price-pill.up .price-popover { top: auto; bottom: 38px; }
.price-popover strong { font-size: 13px; }
.price-popover p { margin: 0; color: var(--portal-text-soft); }
.price-popover .price-error { color: var(--portal-danger); }
.price-popover small { color: var(--portal-faint); line-height: 16px; }
</style>
