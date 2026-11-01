<script setup lang="ts">
/**
 * 规划确认卡（`submit_plan`，设计 03 §3.4 C —— 最关键的一张）。
 *
 * - **逐项列费用**、合计、当前余额 → 确认后预计剩余；确认按钮上直接写金额（对比 XYQ-101 确认时不给费用）。
 * - 金额**全部来自服务端 payload**（`planCardView` 只拼措辞，不推算）。
 * - 余额不足：按钮置灰、写「余额不足，还差 ✦x」+「去充值」（新标签页打开，回来时按最新余额重算）。
 * - 价格变了：服务端回 409 `reason=price_changed` 并把新报价写回卡片，这里提示「价格已更新」，
 *   由用户核对后再点一次 —— 不替他自动确认一个他没看过的价。
 * - 「需要调整」：展开意见框，提交后此卡作废、Agent 回到规划。
 *
 * 营销会话（MKT-T-24，营销 dev/03 §3.1；条目字段见 MKT-T-20 `AgentPlanItem`，逻辑在 `utils/planItems`）：
 * - `notices`（口播类的如实提示，服务端追加）**置顶**；
 * - 营销条目多显示变体标签、选项、选用的商品图缩略图（角标 = 发给模型的顺序）与「共 X 张，本项用 Y 张（模型上限 L）」；
 * - 「更换」打开 `ProductImagePicker`，选好的序号随批准以 `itemOverrides` 提交。换了图**不在页面估价**：
 *   服务端重报价，合计不变直接批准，变了回 409 + 新报价（已按换后的图写回卡片）→ 卡片刷新、用户再确认一次。
 * - 通用会话的卡片没有这些字段，渲染与提交体与改动前一致（`{decision:"approve"}`）。
 *
 * 批量 / 裂变（MKT-T-29，MKT-F-50 / 53、MKT-AC-17）：
 * - 两个以上变体时按变体分组（组头「变体 N · 说明」+ 本组小计），每一行仍是服务端的逐项报价；
 *   **合计只显示服务端的 `total`**，不在页面上另算一个。
 * - 素材裂变条目显示服务端写的 `fissionNote`（走视频生视频还是重新生成、为什么、本项花多少）。
 * - 批准回 42215：`details.key` 指向的那一项标红并滚到它（之前只有一句错误原话，用户要自己去找是哪一项）。
 */
import { FileText, ImageIcon } from "@lucide/vue";
import CardSummary from "./CardSummary.vue";
import FileCard from "../messages/FileCard.vue";
import ProductImagePicker from "../../composer/marketing/ProductImagePicker.vue";
import { credits, planCardView, type AgentCard, type PlanItemView } from "~/utils/agentMessages";
import { apiFailure } from "~/utils/agentPanelBridge";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { marketingProductTitle } from "~/utils/marketingLibrary";
import {
  marketingPlanItem,
  planApproveSubmission,
  planItemOverrides,
  planItemProductText,
  planFailedItemKey,
  planNotices,
  planProductMedia,
  planVariantGroups
} from "~/utils/planItems";

const props = defineProps<{ card: AgentCard; readOnlyReason?: string; submit: (submission: Record<string, unknown>) => Promise<void> }>();
const generation = useGenerationStore();
const agent = useAgentStore();

const liveBalance = ref<number>();
const view = computed(() => planCardView(props.card.payload, liveBalance.value));
const adjusting = ref(false);
const comment = ref("");
const busy = ref(false);
const error = ref("");
const notice = ref("");

const rawItems = computed<unknown[]>(() => {
  const items = (props.card.payload as { items?: unknown } | null | undefined)?.items;
  return Array.isArray(items) ? items : [];
});
const notices = computed(() => planNotices(props.card.payload));
/** 变体分组（两个以上变体才分组）；与 `view.items` 同下标。 */
const groups = computed(() => planVariantGroups(rawItems.value));
/** 渲染顺序：分组时「组头 + 组内条目」，否则原样平铺。 */
type PlanItemRow = { kind: "item"; key: string; index: number; item: PlanItemView };
const rows = computed<Array<{ kind: "group"; key: string; title: string; subtotal: string } | PlanItemRow>>(() => {
  const itemRow = (index: number): PlanItemRow[] => {
    const item = view.value.items[index];
    return item ? [{ kind: "item", key: `i${index}`, index, item }] : [];
  };
  if (!groups.value.length) return view.value.items.flatMap((_, index) => itemRow(index));
  return groups.value.flatMap(group => [
    { kind: "group" as const, key: `g${group.variant}`, title: group.title, subtotal: credits(group.subtotal) },
    ...group.indexes.flatMap(itemRow)
  ]);
});
/** 批准时被服务端点名（42215）的条目键：标红，用户改了规划（卡片条目变了）就撤掉。 */
const failedKey = ref("");
const root = ref<HTMLElement>();
/** 每一项的营销信息（通用条目为 undefined）；与 `view.items` 同下标。 */
const marketing = computed(() => rawItems.value.map(item => marketingPlanItem(item)));
/** 会话的商品图（序号 i 对应 products[i-1]）；不是营销会话为空。 */
const products = computed(() => agent.detail?.sceneInput?.products ?? []);
/** 选图区标题「商品：<名称>」（快照里的名称，MKT-F-99）；旧会话没有名称时不显示。 */
const productTitle = computed(() => marketingProductTitle(agent.detail?.sceneInput?.product?.name));

/** 用户在这张卡上换过的图：条目键 → 序号（按发送顺序）。 */
const selections = ref<Record<string, number[]>>({});
/** 正在换图的条目键。 */
const changing = ref<string | null>(null);
const overrides = computed(() => planItemOverrides(rawItems.value, selections.value));

/** 某一项此刻显示的选图（换过的以换后的为准 —— 界面上看到的就是随后发出去的）。 */
function selectionOf(index: number): { indexes: number[]; mediaIds: string[]; changed: boolean } {
  const item = marketing.value[index];
  if (!item) return { indexes: [], mediaIds: [], changed: false };
  const picked = selections.value[item.key];
  if (picked && overrides.value.some(o => o.key === item.key)) {
    return { indexes: picked, mediaIds: planProductMedia(products.value, picked), changed: true };
  }
  return { indexes: item.productIndexes, mediaIds: item.productMediaIds, changed: false };
}

// 受保护媒体：卡片里的缩略图与选择器各持一份，卸载时随作用域 revoke。
const thumbs = useAuthenticatedMedia();
watch(() => marketing.value.flatMap((_, i) => selectionOf(i).mediaIds), ids => {
  void thumbs.appendAll([...new Set(ids)].map(id => ({ key: id, url: mediaThumbUrl(id, 160) })));
}, { immediate: true });

/** 从充值页回来：重新取余额（卡片上的余额是报价那一刻的）。 */
async function refreshBalance() {
  if (typeof document !== "undefined" && document.visibilityState !== "visible") return;
  await generation.loadBalance();
  liveBalance.value = generation.balance;
}
onMounted(() => document.addEventListener("visibilitychange", refreshBalance));
onBeforeUnmount(() => document.removeEventListener("visibilitychange", refreshBalance));

// 卡片被重新报价（payload 变了）：以服务端新给的余额为准
watch(() => props.card.payload, () => { liveBalance.value = undefined; });
// 条目真的变了（409 后服务端按换后的图写回了新报价）：本地的换图选择作废 —— 卡片上已经是换后的结果。
// 只看条目内容不看对象身份：每次对账都会换一个新对象，按身份清的话用户选到一半就被清掉。
watch(() => JSON.stringify(rawItems.value), (next, prev) => {
  if (next === prev) return;
  selections.value = {};
  changing.value = null;
  failedKey.value = "";
});

async function send(submission: Record<string, unknown>) {
  if (busy.value || props.readOnlyReason) return;
  busy.value = true;
  error.value = "";
  notice.value = "";
  try {
    await props.submit(submission);
  } catch (cause) {
    const failure = apiFailure(cause);
    if (failure.details.reason === "price_changed") notice.value = "价格已更新，请核对新的费用后再确认";
    else if (failure.details.reason === "plan_doc_changed") notice.value = "规划文档在确认前被修改过，这张卡已作废，Agent 会按最新文档重新规划";
    else error.value = failure.message;
    failedKey.value = planFailedItemKey(failure);
    if (failedKey.value) {
      await nextTick();
      root.value?.querySelector(".plan-failed")?.scrollIntoView({ block: "nearest", behavior: "smooth" });
    }
  } finally {
    busy.value = false;
  }
}

function approve() {
  if (!view.value.canApprove || changing.value) return;
  void send(planApproveSubmission(overrides.value));
}

function sendAdjust() {
  const text = comment.value.trim();
  if (!text) return;
  void send({ decision: "adjust", comment: text });
}

function startChange(key: string) {
  if (busy.value || props.readOnlyReason) return;
  changing.value = changing.value === key ? null : key;
}

function applyChange(key: string, indexes: number[]) {
  selections.value = { ...selections.value, [key]: indexes };
  changing.value = null;
}

function resetChange(key: string) {
  selections.value = Object.fromEntries(Object.entries(selections.value).filter(([k]) => k !== key));
}
</script>

<template>
  <CardSummary v-if="card.status !== 'pending'" :title="view.title" :status="card.status" :detail="card.status === 'submitted' && card.submission?.decision === 'approve' ? `已批准 ${view.total}` : undefined" />
  <section v-else ref="root" class="agent-card is-plan" :data-card-id="card.id">
    <header class="agent-card-title">请确认创作规划</header>
    <ul v-if="notices.length" class="plan-notices" role="note">
      <li v-for="text in notices" :key="text" class="agent-notice">{{ text }}</li>
    </ul>
    <FileCard v-if="view.docPath" :path="view.docPath" :subtitle="view.title" />
    <p v-else class="agent-plan-title"><FileText :size="14" />{{ view.title }}</p>

    <div class="agent-plan-section">
      <span class="agent-muted">将要生成</span>
      <ul class="agent-plan-items">
        <template v-for="row in rows" :key="row.key">
        <li v-if="row.kind === 'group'" class="plan-group"><span>{{ row.title }}</span><em>小计 {{ row.subtotal }}</em></li>
        <li
          v-else
          :class="{ 'is-blocked': row.item.blocked, 'is-marketing': marketing[row.index], 'plan-failed': failedKey && marketing[row.index]?.key === failedKey, 'in-group': groups.length }"
        >
          <span>
            <strong>{{ row.item.label }}<small v-if="!groups.length && marketing[row.index]?.variantText" class="plan-variant">{{ marketing[row.index]!.variantText }}</small></strong>
            <small>{{ row.item.meta }}</small>
            <small v-if="marketing[row.index]?.picksText">{{ marketing[row.index]!.picksText }}</small>
            <template v-if="marketing[row.index] && planItemProductText({ productTotal: marketing[row.index]!.productTotal, imageLimit: marketing[row.index]!.imageLimit, selected: selectionOf(row.index).indexes.length })">
              <small v-if="productTitle" class="plan-product-name">{{ productTitle }}</small>
              <span class="plan-products">
                <span v-for="(id, n) in selectionOf(row.index).mediaIds" :key="`${id}-${n}`" class="plan-thumb" :title="`第 ${selectionOf(row.index).indexes[n]} 张`">
                  <img v-if="thumbs.urls.value[id]" :src="thumbs.urls.value[id]" :alt="`商品图 ${selectionOf(row.index).indexes[n]}`">
                  <ImageIcon v-else :size="12" />
                  <i>{{ n + 1 }}</i>
                </span>
              </span>
              <small>
                {{ planItemProductText({ productTotal: marketing[row.index]!.productTotal, imageLimit: marketing[row.index]!.imageLimit, selected: selectionOf(row.index).indexes.length }) }}
                <template v-if="selectionOf(row.index).changed">· 已更换</template>
                <button
                  v-if="marketing[row.index]!.canChange && products.length && !readOnlyReason"
                  type="button"
                  class="plan-change"
                  :disabled="busy"
                  @click="startChange(marketing[row.index]!.key)"
                >{{ changing === marketing[row.index]!.key ? "收起" : "更换" }}</button>
                <button v-if="selectionOf(row.index).changed && !readOnlyReason" type="button" class="plan-change" :disabled="busy" @click="resetChange(marketing[row.index]!.key)">还原</button>
              </small>
              <ProductImagePicker
                v-if="changing === marketing[row.index]!.key"
                :products="products"
                :selected="selectionOf(row.index).indexes"
                :limit="marketing[row.index]!.imageLimit ?? 0"
                :product-name="agent.detail?.sceneInput?.product?.name ?? ''"
                @confirm="indexes => applyChange(marketing[row.index]!.key, indexes)"
                @cancel="changing = null"
              />
            </template>
            <small v-if="marketing[row.index]?.fissionNote" class="plan-fission">{{ marketing[row.index]!.fissionNote }}</small>
            <small v-if="row.item.blocked" class="agent-error-text">{{ row.item.blocked }}</small>
          </span>
          <em>{{ row.item.amount }}</em>
        </li>
        </template>
        <li v-if="!view.items.length" class="agent-muted">（没有计划项）</li>
      </ul>
      <div class="agent-plan-total"><span>合计</span><strong>{{ view.total }}</strong></div>
      <p class="agent-plan-balance">
        当前余额 {{ view.balance }}
        <template v-if="view.remaining"> → 确认后预计剩余 {{ view.remaining }}</template>
      </p>
      <p v-if="overrides.length" class="agent-muted plan-reprice">已更换 {{ overrides.length }} 项的商品图：确认时按新选择重新报价，费用有变化会先请你再确认一次</p>
    </div>

    <p class="agent-muted agent-plan-note">{{ view.budgetNote }}</p>
    <p v-if="notice" class="agent-notice">{{ notice }}</p>
    <p v-if="error" class="agent-error-text">{{ error }}</p>

    <div v-if="adjusting" class="agent-plan-adjust">
      <textarea v-model="comment" class="agent-textarea" rows="3" maxlength="2000" placeholder="想怎么调整？例如：改成 10 秒、换成水墨风" />
      <div class="agent-card-actions">
        <button type="button" class="agent-secondary" @click="adjusting = false">收起</button>
        <button type="button" class="agent-primary" :disabled="!comment.trim() || busy" @click="sendAdjust">提交修改意见</button>
      </div>
    </div>

    <footer v-if="readOnlyReason" class="agent-card-actions">
      <span class="agent-muted">{{ readOnlyReason }}</span>
    </footer>
    <footer v-else-if="!adjusting" class="agent-card-actions is-split">
      <button type="button" class="agent-secondary" :disabled="busy" @click="adjusting = true">需要调整</button>
      <span class="agent-plan-confirm">
        <small v-if="view.blockedText" class="agent-error-text">{{ view.blockedText }}</small>
        <small v-else-if="changing" class="agent-muted">先确定或取消换图</small>
        <NuxtLink v-if="view.shortfall" to="/credits" target="_blank" class="agent-link-button">去充值</NuxtLink>
        <button type="button" class="agent-primary" :disabled="!view.canApprove || busy || Boolean(changing)" :title="view.blockedText || ''" @click="approve">{{ view.approveText }}</button>
      </span>
    </footer>
  </section>
</template>

<style scoped>
/* 营销条目（MKT-T-24）：通用卡片不出现这些元素，样式与改动前一致 */
.plan-notices { margin: 0; padding: 0; list-style: none; display: grid; gap: 4px; }
.plan-variant { margin-left: 6px; font-weight: 400; }
.plan-product-name { color: var(--canvas-text-soft, var(--portal-text-soft)); }
.plan-products { display: flex; flex-wrap: wrap; gap: 4px; margin: 3px 0 1px; }
.plan-thumb {
  position: relative;
  width: 32px;
  height: 32px;
  display: grid;
  place-items: center;
  overflow: hidden;
  border: 1px solid var(--canvas-line, var(--portal-line));
  border-radius: 6px;
  color: var(--canvas-faint, var(--portal-muted));
}
.plan-thumb img { width: 100%; height: 100%; object-fit: cover; }
.plan-thumb i {
  position: absolute;
  left: 1px;
  top: 1px;
  min-width: 12px;
  height: 12px;
  padding: 0 2px;
  border-radius: 6px;
  color: var(--cv-n06, var(--portal-mark-text));
  background: var(--cv-n94, var(--portal-mark-bg));
  font-size: 9px;
  font-style: normal;
  line-height: 12px;
  text-align: center;
}
.plan-change {
  margin-left: 6px;
  padding: 0;
  border: 0;
  /* 中性色（与面板统一，design/03 R-3）：不靠颜色区分，用下划线提示可点。 */
  color: var(--canvas-text, var(--portal-text));
  background: none;
  font: inherit;
  text-decoration: underline;
  text-decoration-color: var(--canvas-line-strong, var(--portal-line-strong));
  text-underline-offset: 2px;
  cursor: pointer;
}
.plan-change:hover:not(:disabled) { text-decoration-color: currentColor; }
.plan-change:disabled { color: var(--canvas-faint, var(--portal-muted)); cursor: not-allowed; }
.plan-reprice { margin: 0; font-size: 11px; }
/* 批量 / 裂变（MKT-T-29） */
.agent-plan-items .plan-group {
  display: flex;
  justify-content: space-between;
  gap: 8px;
  margin-top: 4px;
  padding: 4px 0 2px;
  border-bottom: 1px dashed var(--canvas-line, var(--portal-line));
  color: var(--canvas-text, var(--portal-text));
  font-size: 12px;
  font-weight: 600;
}
.agent-plan-items .plan-group em { font-style: normal; font-weight: 400; color: var(--canvas-faint, var(--portal-muted)); }
.agent-plan-items .in-group { padding-left: 10px; }
.plan-fission { color: var(--canvas-faint, var(--portal-muted)); }
.agent-plan-items .plan-failed {
  border-radius: 6px;
  outline: 1px solid var(--canvas-danger, var(--portal-danger));
  outline-offset: 2px;
}
</style>
