<script setup lang="ts">
/**
 * 片段级生成的通用确认框（docs/agent/smartcut SCT-40，dev/03 §5.6，SC-UX-05、SC-N-12）。五个工具的表单都套它：
 * 表单给出请求（不完整时为 null）→ 这里按请求取服务端报价（dryRun，只判可用性，TT-T-14 不显示金额）→ 写明发给哪家服务处理
 * → 确认后带 `Idempotency-Key` 受理。
 *
 * - `blockers` 非空时确认置灰并原样说明原因；
 * - 一次确认一个键：网络抖了一下点「确认」重发时复用同一个键（不会重复受理）；请求一变，键作废；
 * - 受理之后不在前端替换片段：等服务端回填推 `timeline.updated`（SCR-13）。
 */
import { ElMessage } from "element-plus";
import type { components } from "@tabtv/api-client";
import { SmartcutFlushError, type SmartcutClipRequest, type SmartcutStoreGeneration } from "~/stores/smartcut";
import { smartcutClipQuoteText, smartcutClipToolKey, smartcutClipToolLabels } from "~/utils/smartcutClipGen";

type Quote = components["schemas"]["SmartcutQuote"];

const props = defineProps<{
  /** 请求；表单还没填完整时为 null（不报价、不能确认）。 */
  request: SmartcutClipRequest | null;
  /** 发给哪家服务处理（模型名）。 */
  modelName: string;
  /** 表单自己的问题（不报价也知道受理不了：片段长度不在范围里等）。 */
  formReason?: string;
  /** 受理成功后的一句话（缺省按工具说）。 */
  doneText?: string;
}>();
const emit = defineEmits<{ close: []; done: [generation: SmartcutStoreGeneration] }>();
const store = useSmartcutStore();

const quote = ref<Quote | null>(null);
const quoting = ref(false);
const busy = ref(false);
const failure = ref("");
let idemKey = "";
let quoteSeq = 0;
let timer: ReturnType<typeof setTimeout> | undefined;

const tool = computed(() => props.request?.tool);
const title = computed(() => (tool.value ? smartcutClipToolLabels[tool.value] : "片段级生成"));
const text = computed(() => smartcutClipQuoteText(quote.value, props.modelName));
const blocked = computed(() => props.formReason || text.value.blocked);
const confirmDisabled = computed(() => busy.value || quoting.value || !props.request || !quote.value || Boolean(blocked.value));
const confirmLabel = computed(() => (busy.value ? "提交中…" : "确认"));

function errorText(cause: unknown, fallback: string): string {
  const message = (cause as { data?: { message?: unknown }; message?: unknown } | undefined);
  const text = typeof message?.data?.message === "string" ? message.data.message : typeof message?.message === "string" ? message.message : "";
  return text && text !== "请求失败" ? text : fallback;
}

async function refreshQuote() {
  const req = props.request;
  const seq = ++quoteSeq;
  if (!req || props.formReason) {
    quote.value = null;
    quoting.value = false;
    return;
  }
  quoting.value = true;
  failure.value = "";
  try {
    const q = await store.quoteClipTool(req);
    if (seq === quoteSeq) quote.value = q;
  } catch (cause) {
    if (seq === quoteSeq) {
      quote.value = null;
      failure.value = cause instanceof SmartcutFlushError ? cause.message : errorText(cause, "报价失败，请检查网络后重试");
    }
  } finally {
    if (seq === quoteSeq) quoting.value = false;
  }
}

// 请求一变就重新报价（打字时稍等一下再报），同一次确认的幂等键也作废：内容不同就是另一次请求。
watch(() => [JSON.stringify(props.request), props.formReason], () => {
  idemKey = "";
  if (timer) clearTimeout(timer);
  timer = setTimeout(() => void refreshQuote(), 300);
}, { immediate: true });
onBeforeUnmount(() => { if (timer) clearTimeout(timer); });

const DONE: Record<string, string> = {
  regenerate: "已开始重新生成：完成后这一段会自动换上新结果，原素材进备选",
  fill: "已开始补镜：完成后插在选定的位置",
  desubtitle: "已开始去字幕：完成后这一段会自动换上去掉字幕的版本，原素材进备选",
  music: "已开始生成配乐：完成后放进配乐轨",
  sfx: "已开始生成音效：完成后放进音效轨，跟随这一段"
};

async function confirm() {
  const req = props.request;
  if (confirmDisabled.value || !req) return;
  busy.value = true;
  failure.value = "";
  try {
    idemKey ||= smartcutClipToolKey(req.tool);
    const res = await store.runClipTool(req, idemKey);
    idemKey = "";
    ElMessage.success(props.doneText || DONE[req.tool] || "已开始生成");
    emit("done", res);
    emit("close");
  } catch (cause) {
    failure.value = cause instanceof SmartcutFlushError ? cause.message : errorText(cause, "没有发起，请重试");
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <div class="sc-modal-backdrop" data-testid="smartcut-clip-tool-dialog" :data-tool="tool" @click.self="emit('close')">
    <section class="sc-modal" role="dialog" aria-modal="true" aria-labelledby="sc-clip-tool-title">
      <h2 id="sc-clip-tool-title">{{ title }}</h2>
      <div class="sc-section">
        <slot />
      </div>
      <div class="sc-section" aria-live="polite">
        <p v-if="formReason" class="sc-error" data-testid="smartcut-clip-tool-reason">{{ formReason }}</p>
        <template v-else>
          <p v-if="quoting" class="sc-note">报价中…</p>
          <p v-else-if="text.blocked" class="sc-error" data-testid="smartcut-clip-tool-reason">{{ text.blocked }}</p>
        </template>
        <p class="sc-note" data-testid="smartcut-clip-tool-provider">{{ text.provider }}。</p>
        <p v-if="failure" class="sc-error" role="alert">{{ failure }}</p>
      </div>
      <div class="sc-modal-actions">
        <button type="button" class="sc-btn" @click="emit('close')">取消</button>
        <button type="button" class="sc-btn primary" :disabled="confirmDisabled" :title="blocked || ''" data-testid="smartcut-clip-tool-confirm" @click="confirm">
          {{ confirmLabel }}
        </button>
      </div>
    </section>
  </div>
</template>
