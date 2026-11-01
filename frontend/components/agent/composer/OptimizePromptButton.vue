<script setup lang="ts">
/**
 * 一键优化提示词（AGT-43，设计 02 §4.1「✎」）：输入框右上角。
 *
 * 流程：点按钮 → 浮层先说清**这次要花多少**（按轮计费，与 Agent 对话同价）→ 用户点「优化」才调
 * `POST /agent/prompt-optimize` → 结果以「原文 / 优化后」对比给出，「替换」才写回输入框，「保留原文」什么都不改。
 *
 * ⚠️ **不自动覆盖**：模型改写可能丢掉用户在意的细节，替换与否由用户决定。
 * ⚠️ 这是付费调用：每次点「优化」换一个新的幂等键（每次都是一次新的改写、单独计费）；
 *    同一次请求的网络重试才复用键（这里不自动重试，由用户自己再点）。
 * ⚠️ 失败时说服务端原话（其中带「已全额退回 ✦N」）；断网说断网。
 * ⚠️ 打开页面、打开浮层都不发请求：只有点「优化」那一下才花钱。
 * ⚠️ `purpose`（MKT-T-39「AI 帮写」用 `character_appearance`）**不传时请求体逐字不变**（`optimizeRequestBody`）；
 *    `label` / `placement` 只改外观（按钮带字、浮层向上弹），不传时与原来一样。
 */
import { Check, LoaderCircle, Wand2, X } from "@lucide/vue";
import { optimizeRequestBody, type OptimizePriceView, type OptimizePurpose } from "~/utils/composerPrice";
import { quickErrorText } from "~/utils/quickGenerate";

type OptimizeResult = { id: string; text: string; modelCode: string; credits: number; replayed: boolean };

const props = defineProps<{
  /** 输入框当前正文。 */
  text: string;
  /** 要用的推理模型（与 Agent 对话同一个）；不给由服务端取第一个可用的。 */
  modelCode?: string;
  price: OptimizePriceView;
  /** 不能用的原因（只读、正在发送…）；为空可用。 */
  blockReason?: string;
  /** 点之前的检查（未登录时去登录）；返回 false 就不打开。 */
  guard?: () => boolean | Promise<boolean>;
  /** 改写用途；不传 = 一键优化（请求体不带这个字段）。 */
  purpose?: OptimizePurpose;
  /** 按钮上的文字（如「AI 帮写」）；不传 = 只有图标。 */
  label?: string;
  /** 浮层弹出方向；默认向下。 */
  placement?: "bottom" | "top";
}>();
const title = computed(() => props.label || "一键优化提示词");
const intro = computed(() => (props.purpose === "character_appearance"
  ? "让推理模型把形象描述写得更具体（五官、表情、服饰、材质、体型等），结果先给你对比，确认后才替换。"
  : "让推理模型把你的描述改写得更具体，结果先给你对比，确认后才替换。"));
const emit = defineEmits<{
  /** 用户选了「替换」。 */
  replace: [text: string];
  /** 发生过一次计费调用（成功或失败），父组件据此刷新余额。 */
  charged: [];
}>();

type Phase = "closed" | "confirm" | "running" | "result" | "error";
const phase = ref<Phase>("closed");
const result = ref("");
const credits = ref(0);
const error = ref("");
/** 发起优化时的原文：对比与「输入框改过了」的提示都以它为准。 */
const original = ref("");
const root = ref<HTMLElement>();

const disabledReason = computed(() => {
  if (props.blockReason) return props.blockReason;
  if (!props.text.trim()) return "先写下想法，再让 AI 帮你润色";
  if (props.price.error) return props.price.error;
  return "";
});
const editedSince = computed(() => phase.value === "result" && props.text !== original.value);

async function toggle() {
  if (phase.value === "running") return;
  if (phase.value !== "closed") {
    phase.value = "closed";
    return;
  }
  if (props.guard && !(await props.guard())) return;
  if (disabledReason.value) return;
  error.value = "";
  phase.value = "confirm";
}

async function run() {
  if (disabledReason.value || phase.value === "running") return;
  original.value = props.text;
  phase.value = "running";
  error.value = "";
  const key = globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
  try {
    const response = await useApi()<OptimizeResult>("/agent/prompt-optimize", {
      method: "POST",
      headers: { "Idempotency-Key": key },
      body: optimizeRequestBody(original.value, props.modelCode, props.purpose)
    });
    result.value = response.text;
    credits.value = response.credits;
    phase.value = "result";
  } catch (cause) {
    error.value = quickErrorText(cause, navigator.onLine);
    phase.value = "error";
  } finally {
    emit("charged");
  }
}

function accept() {
  emit("replace", result.value);
  phase.value = "closed";
}

function keep() {
  phase.value = "closed";
}

function onOutside(event: PointerEvent) {
  // 运行中不因点别处而关掉：结果回来时浮层不在了，用户以为钱白花了。
  if (phase.value === "running" || phase.value === "result") return;
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) phase.value = "closed";
}
function onKey(event: KeyboardEvent) {
  if (event.key === "Escape" && phase.value !== "running") phase.value = "closed";
}
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
</script>

<template>
  <div ref="root" class="optimize" :class="{ 'with-label': label, 'pop-top': placement === 'top' }" @keydown="onKey">
    <button
      type="button"
      class="optimize-trigger"
      :class="{ active: phase !== 'closed' }"
      :disabled="phase === 'running'"
      :aria-disabled="Boolean(disabledReason)"
      :aria-expanded="phase !== 'closed'"
      :title="disabledReason || `${title}${price.label ? `（${price.label}/次）` : ''}`"
      :aria-label="title"
      @click.stop="toggle"
    >
      <LoaderCircle v-if="phase === 'running'" :size="15" class="spin" />
      <Wand2 v-else :size="15" />
      <span v-if="label">{{ label }}</span>
    </button>
    <div v-if="phase !== 'closed'" class="optimize-popover" role="dialog" :aria-label="title">
      <template v-if="phase === 'confirm'">
        <strong>{{ title }}</strong>
        <p>{{ intro }}</p>
        <p class="optimize-price">{{ price.text || '按轮计费' }}</p>
        <div class="optimize-actions">
          <button type="button" class="ghost" @click="keep">取消</button>
          <button type="button" class="primary" :disabled="Boolean(disabledReason)" @click="run">
            优化<template v-if="price.label">（{{ price.label }}）</template>
          </button>
        </div>
      </template>
      <template v-else-if="phase === 'running'">
        <strong>正在优化…</strong>
        <p>通常十几秒，请稍候；结果出来后由你决定是否替换。</p>
      </template>
      <template v-else-if="phase === 'error'">
        <strong>这次没有优化成功</strong>
        <p class="optimize-error" role="alert">{{ error }}</p>
        <div class="optimize-actions">
          <button type="button" class="ghost" @click="keep">关闭</button>
          <button type="button" class="primary" :disabled="Boolean(disabledReason)" @click="run">
            再试一次<template v-if="price.label">（{{ price.label }}）</template>
          </button>
        </div>
      </template>
      <template v-else>
        <strong>优化结果<small v-if="credits">（已扣 ✦{{ credits }}）</small></strong>
        <div class="optimize-compare">
          <section>
            <h4>原文</h4>
            <p>{{ original }}</p>
          </section>
          <section class="after">
            <h4>优化后</h4>
            <p>{{ result }}</p>
          </section>
        </div>
        <p v-if="editedSince" class="optimize-warn">输入框在优化之后又改过，替换会覆盖这些改动。</p>
        <div class="optimize-actions">
          <button type="button" class="ghost" @click="keep"><X :size="13" />保留原文</button>
          <button type="button" class="primary" @click="accept"><Check :size="13" />替换</button>
        </div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.optimize { position: relative; }
.optimize-trigger {
  width: 28px;
  height: 28px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 8px;
  color: var(--portal-text-soft);
  background: transparent;
  cursor: pointer;
}
.optimize-trigger:hover:not(:disabled), .optimize-trigger.active { color: var(--portal-text); background: var(--portal-active); }
.optimize-trigger[aria-disabled="true"] { color: var(--portal-disabled-text); }
.optimize-trigger[aria-disabled="true"]:hover { color: var(--portal-disabled-text); background: transparent; cursor: not-allowed; }
.optimize-trigger:disabled { cursor: progress; }
.optimize.with-label .optimize-trigger {
  width: auto;
  height: 32px;
  display: inline-flex;
  gap: 5px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 999px;
  padding: 0 12px;
  color: var(--portal-text);
  font: inherit;
  font-size: 12px;
  font-weight: 600;
}
.optimize.pop-top .optimize-popover { top: auto; bottom: calc(100% + 6px); right: auto; left: 0; }
.optimize-popover {
  width: min(460px, calc(100vw - 32px));
  position: absolute;
  top: calc(100% + 6px);
  right: 0;
  z-index: 90;
  display: grid;
  gap: 8px;
  padding: 14px 16px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 14px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 18px 48px var(--portal-shadow-strong);
  font-size: 12px;
}
.optimize-popover strong { font-size: 13px; }
.optimize-popover strong small { margin-left: 4px; color: var(--portal-muted); font-weight: 400; }
.optimize-popover p { margin: 0; color: var(--portal-text-soft); line-height: 18px; }
.optimize-price { color: var(--portal-text) !important; }
.optimize-error { color: var(--portal-danger) !important; }
.optimize-warn { color: var(--portal-gold) !important; }
.optimize-compare { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
@media (max-width: 560px) { .optimize-compare { grid-template-columns: 1fr; } }
.optimize-compare section {
  min-width: 0;
  max-height: 220px;
  overflow-y: auto;
  border: 1px solid var(--portal-line);
  border-radius: 10px;
  padding: 8px 10px;
  background: var(--portal-surface);
}
.optimize-compare section.after { border-color: var(--portal-line-strong); }
.optimize-compare h4 { margin: 0 0 4px; color: var(--portal-muted); font-size: 11px; font-weight: 500; }
.optimize-compare p { color: var(--portal-text); white-space: pre-wrap; word-break: break-word; }
.optimize-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 2px; }
.optimize-actions button {
  height: 28px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: 0;
  border-radius: 8px;
  padding: 0 12px;
  font: inherit;
  font-size: 12px;
  cursor: pointer;
}
.optimize-actions .ghost { color: var(--portal-text-soft); background: var(--portal-surface-2); }
.optimize-actions .ghost:hover { color: var(--portal-text); background: var(--portal-surface-3); }
.optimize-actions .primary { color: var(--portal-mark-text); background: var(--portal-mark-bg); }
.optimize-actions .primary:disabled { color: var(--portal-disabled-text); background: var(--portal-disabled-bg); cursor: not-allowed; }
.spin { animation: optimize-spin 1s linear infinite; }
@keyframes optimize-spin { to { transform: rotate(360deg); } }
</style>
