<script setup lang="ts">
/**
 * 智能剪辑的指令面板（SCT-23，SCD-48）：选中智能剪辑节点时挂在卡片下方，代替通用参数面板。
 *
 * - 素材条（服务端 `GET .../inputs`）；「有 N 段新素材未使用」→「追加到末尾」打开剪辑台并追加（SC-F-14）；
 * - 「+ 参考」两条路：从画布选（连一条线）、上传（建一个带「已有产出」的上传节点再连线）—— 都只改画布，
 *   画布保存成功后由画布按连线变化同步一次（SCD-49），素材条随后刷新；
 * - 要求、剪辑方式、规格、方案数：节点参数配置驱动（`configStore.nodeType("smartcut")`），改动走 markTouched + updateParams；
 * - 「本小时已用 N / M 次」来自 `/smartcut/config`（tabtv 不计价，原「免费」角标删除，TT-T-14）；「发送」不在这里发请求：打开剪辑台并带 `pendingAction: plan`。
 * - 发送的禁用原因（SC-UX-05）：只读、AI 剪辑进行中（摘要的 `activeCall`）、没有素材、不可用、被限流（说多久后再试，
 *   时间来自剪辑台里上一次 429 的 `retry_after_seconds`，SC-AC-35）。
 * - 剪辑台里点「去连音频节点 / 去连画面」时收起剪辑台、选中本节点，这里打开「+ 参考 → 从画布选」（`useSmartcutLocate`）。
 */
import { ArrowUp, Link2, Maximize2, Plus } from "@lucide/vue";
import { ElMessage } from "element-plus";
import type { components } from "@tabtv/api-client";
import DynamicControl from "../DynamicControl.vue";
import GenerationSizeControl, { type FormatParam } from "../GenerationSizeControl.vue";
import SmartcutModeControl from "./SmartcutModeControl.vue";
import ReferenceUploadButton from "../ReferenceUploadButton.vue";
import InputStrip from "~/components/smartcut/panels/InputStrip.vue";
import type { CanvasNodeData, JsonObject } from "~/types/canvas";
import { asObject, type UploadedReference } from "~/utils/canvas";
import { directorCanEdit } from "~/utils/directorRole";
import { smartcutConnectableNodes, smartcutSendDisabledReason, smartcutTimelineIdOf } from "~/utils/smartcutCanvas";
import { smartcutPublishedGeneration } from "~/utils/smartcutPublish";
import { useSmartcutLocate } from "~/composables/useSmartcutLocate";

type Input = components["schemas"]["SmartcutInput"];

const props = defineProps<{ nodeId: string; data: CanvasNodeData }>();
const emit = defineEmits<{ send: []; "append-unused": []; open: [] }>();
const canvas = useCanvasStore();
const configStore = useCanvasConfigStore();
const workspace = useWorkspaceStore();
const generation = useGenerationStore();
const smartcuts = useSmartcuts();
const smartcutConfig = useSmartcutConfig();
/** AI 关着时只留「+ 参考」、素材条与打开剪辑台：要求输入框、剪辑方式、规格、发送都只对 AI 剪辑有意义。 */
const features = useSmartcutFeatures();

const canEdit = computed(() => directorCanEdit(workspace.current?.role));
const summary = computed(() => smartcuts.get(props.nodeId));
const timelineId = computed(() => summary.value?.timelineId ?? smartcutTimelineIdOf(props.data.params));
const params = computed(() => (configStore.nodeType("smartcut")?.params ?? []).filter(p => p.code !== "prompt").sort((a, b) => (a.sort ?? 0) - (b.sort ?? 0)));
const promptConfig = computed(() => asObject(configStore.nodeType("smartcut")?.params?.find(p => p.code === "prompt")?.config));
const placeholder = computed(() => String(promptConfig.value.placeholder ?? "").trim() || "说说想剪成什么样");
const prompt = computed(() => String(props.data.params.prompt ?? ""));

// ---------------------------------------------------------------- 素材条

const inputs = ref<Input[]>([]);
const omitted = ref(0);
let inputsSeq = 0;
async function loadInputs() {
  const tid = timelineId.value;
  if (!tid || !canvas.projectId) return;
  const seq = ++inputsSeq;
  try {
    const res = await useApi()<{ items: Input[]; omitted?: number }>(`/projects/${canvas.projectId}/smartcuts/${tid}/inputs`);
    if (seq !== inputsSeq) return;
    inputs.value = res.items ?? [];
    omitted.value = res.omitted ?? 0;
  } catch {
    // 取不到素材条不打扰：上一次的还显示着，发送按钮的判断按它来
  }
}
// 摘要版本变了（同步铺了新素材、剪辑台存过）或连线变了（画布存完之后服务端才认得新连线）就重取一次
watch(() => [timelineId.value, summary.value?.version, canvas.saveState === "saved" ? canvas.edges.filter(e => e.target === props.nodeId).length : -1], () => { void loadInputs(); }, { immediate: true });

const readyInputs = computed(() => inputs.value.filter(i => i.status === "ready" && ["video", "image", "audio", "shotGroup"].includes(i.kind)).length);
const hasVoice = computed(() => inputs.value.some(i => i.status === "ready" && (i.audioRole === "voice" || (i.kind === "video" && i.hasAudio))));

// ---------------------------------------------------------------- 参数

/**
 * 底部一行只放两个下拉：剪辑方式 + 规格（画幅 · 分辨率 · 目标时长 · 方案数收进「尺寸」弹窗，同图片 / 视频节点）。
 * 认不出的参数（后台以后加的）仍按 DynamicControl 平铺在两者之后 —— 配置可以比前端新。
 */
const modeParam = computed(() => params.value.find(p => p.code === "smartcut_mode"));
const FORMAT_CODES = new Set(["aspect_ratio", "resolution", "target_duration", "plan_variants"]);
const formatParams = computed<FormatParam[]>(() => params.value.filter(p => FORMAT_CODES.has(p.code ?? "")).map(p => ({
  code: p.code ?? "",
  name: p.name ?? p.code ?? "",
  config: asObject(p.config),
  value: props.data.params[p.code ?? ""],
  defaultValue: p.defaultValue
})));
const otherParams = computed(() => params.value.filter(p => p.code !== "smartcut_mode" && !FORMAT_CODES.has(p.code ?? "")));

function editParam(code: string, value: unknown) {
  canvas.markTouched(props.nodeId);
  canvas.updateParams(props.nodeId, { ...props.data.params, [code]: value } as JsonObject);
}

// ---------------------------------------------------------------- + 参考

/** 「+ 参考」弹出的菜单：root = 两个入口（从画布选 / 上传），canvas = 画布上能连进来的节点。 */
const refMenu = ref<"closed" | "root" | "canvas">("closed");
const pickOpen = computed({
  get: () => refMenu.value === "canvas",
  set: (open: boolean) => { refMenu.value = open ? "canvas" : "closed"; }
});
const refRoot = ref<HTMLElement>();
function onOutside(event: PointerEvent) {
  if (refRoot.value && event.target instanceof Node && !refRoot.value.contains(event.target)) refMenu.value = "closed";
}
onMounted(() => document.addEventListener("pointerdown", onOutside));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside));
const candidates = computed(() => smartcutConnectableNodes(canvas.nodes, canvas.edges, props.nodeId, configStore.config?.connectionRules ?? []));
/** 剪辑台带过来的「去连……」：打开从画布选，并提示要连什么。 */
const locate = useSmartcutLocate();
const locateHint = ref("");
watch(() => locate.pending.value, () => {
  const request = locate.consume(props.nodeId);
  if (!request) return;
  locateHint.value = request.kind === "audio" ? "连一个音频节点进来（配乐或配音）" : "连更多视频或图片节点进来";
  if (canEdit.value) pickOpen.value = true;
}, { immediate: true });
watch(pickOpen, (open) => { if (!open) locateHint.value = ""; });

function connect(sourceId: string) {
  pickOpen.value = false;
  canvas.addEdge(sourceId, props.nodeId);
  ElMessage.success("已连入，画布保存后会同步到素材条");
}

/** 上传：每份建一个带「已有产出」的上传节点（放在智能剪辑左边），再连线 —— 一次撤销撤掉整批。 */
async function onUploaded(list: UploadedReference[]) {
  refMenu.value = "closed";
  if (!list.length) return;
  const self = canvas.nodes.find(node => node.id === props.nodeId);
  const origin = self?.position ?? { x: 400, y: 200 };
  canvas.snapshot();
  const created: string[] = [];
  list.forEach((ref, index) => {
    const id = canvas.addNode(ref.type, ref.name || "上传素材", {}, {
      position: { x: origin.x - 420, y: origin.y + index * 220 }, exactTitle: true, recordHistory: false, persist: false
    });
    canvas.updateGeneration(id, { ...smartcutPublishedGeneration(ref.id), outputType: ref.type });
    canvas.addEdge(id, props.nodeId, { recordHistory: false, persist: false });
    created.push(id);
  });
  canvas.scheduleSave();
  await nextTick();
  for (const id of created) {
    const node = canvas.nodes.find(item => item.id === id);
    if (node) void generation.restoreNode(node).catch(() => undefined);
  }
}

// ---------------------------------------------------------------- 价格与发送

const usage = computed(() => (smartcutConfig.config.value ? `本小时已用 ${smartcutConfig.config.value.usedThisHour} / ${smartcutConfig.config.value.perHour} 次` : ""));
// 被限流时每秒刷新一次（倒数）；没有被限流时不跑计时器。
const now = ref(Date.now());
let ticker: ReturnType<typeof setInterval> | undefined;
watch(() => smartcutConfig.retryAt.value, (at) => {
  now.value = Date.now();
  clearInterval(ticker);
  ticker = undefined;
  if (at > now.value) {
    ticker = setInterval(() => {
      now.value = Date.now();
      if (smartcutConfig.retryAt.value <= now.value) {
        clearInterval(ticker);
        ticker = undefined;
      }
    }, 1000);
  }
}, { immediate: true });
onBeforeUnmount(() => clearInterval(ticker));
const sendReason = computed(() => smartcutSendDisabledReason({
  canEdit: canEdit.value,
  running: Boolean(summary.value?.activeCall),
  readyInputs: readyInputs.value,
  config: smartcutConfig.config.value,
  configError: smartcutConfig.state.value === "error" ? smartcutConfig.error.value : undefined,
  mode: String(props.data.params.smartcut_mode ?? "mix"),
  hasVoice: hasVoice.value,
  retryAfterSeconds: smartcutConfig.retryAfterSeconds(now.value)
}));
const appendReason = computed(() => (canEdit.value ? "" : "需要编辑权限：你在该工作区是只读成员"));

function send() {
  if (sendReason.value) {
    ElMessage.warning(sendReason.value);
    return;
  }
  emit("send");
}

onMounted(() => {
  if (features.ai) void smartcutConfig.load();
});
</script>

<template>
  <section class="node-param-panel smartcut-prompt-panel nodrag nowheel" aria-label="智能剪辑指令" data-testid="smartcut-prompt-panel">
    <div class="node-top-controls smartcut-top">
      <div ref="refRoot" class="smartcut-ref-anchor">
        <button
          type="button"
          class="smartcut-ref-chip"
          :disabled="!canEdit"
          :title="canEdit ? '连入素材：从画布选或上传' : '需要编辑权限：你在该工作区是只读成员'"
          aria-label="添加参考素材"
          :aria-expanded="refMenu !== 'closed'"
          data-testid="smartcut-ref-chip"
          @click="refMenu = refMenu === 'closed' ? 'root' : 'closed'"
        ><Plus :size="13" />参考</button>
        <div v-if="refMenu === 'root'" class="smartcut-ref-menu smartcut-ref-entries" role="menu" aria-label="添加参考素材">
          <button type="button" role="menuitem" class="smartcut-ref-entry" aria-label="从画布选参考" @click="refMenu = 'canvas'"><Link2 :size="14" />从画布选</button>
          <ReferenceUploadButton
            :project-id="canvas.projectId"
            :node-id="nodeId"
            :model-value="[]"
            :config="{ accept: 'video/*,image/*,audio/*', maxCount: 9, maxSizeByType: { image: 20971520, video: 524288000, audio: 104857600 } }"
            name="素材"
            label="上传本地素材"
            class="smartcut-ref-entry"
            @update:model-value="onUploaded"
          />
        </div>
        <ul v-else-if="refMenu === 'canvas'" class="smartcut-ref-menu" role="menu" aria-label="可连入的节点" data-testid="smartcut-ref-menu">
          <li v-if="locateHint" class="hint" data-testid="smartcut-locate-hint">{{ locateHint }}</li>
          <li v-if="!candidates.length" class="empty">画布上没有能连进来的节点</li>
          <li v-for="item in candidates.slice(0, 30)" :key="item.id" role="menuitem" tabindex="0" @click="connect(item.id)" @keydown.enter="connect(item.id)">
            {{ item.data.title || item.id }}<small>{{ item.data.nodeType }}</small>
          </li>
        </ul>
      </div>
      <button type="button" class="smartcut-expand" title="打开智能剪辑" aria-label="打开智能剪辑" data-testid="smartcut-panel-open" @click="emit('open')"><Maximize2 :size="15" /></button>
    </div>
    <InputStrip v-if="inputs.length || omitted" class="smartcut-inputs" :items="inputs" :omitted="omitted" :arranged="summary?.isArranged" :can-append="canEdit" :append-reason="appendReason" @append-unused="emit('append-unused')" />
    <template v-if="features.ai">
    <textarea
      :value="prompt"
      :placeholder="placeholder"
      :readonly="!canEdit"
      :rows="Number(promptConfig.rows ?? 3)"
      aria-label="剪辑要求"
      @input="editParam('prompt', ($event.target as HTMLTextAreaElement).value)"
    />
    <div class="node-bottom-controls smartcut-bottom">
      <div class="node-control-scroll">
        <SmartcutModeControl
          v-if="modeParam"
          :config="asObject(modeParam.config)"
          :model-value="data.params.smartcut_mode ?? modeParam.defaultValue"
          :disabled="!canEdit"
          @update:model-value="editParam('smartcut_mode', $event)"
        />
        <GenerationSizeControl node-type="smartcut" :params="formatParams" @update="editParam" />
        <DynamicControl
          v-for="param in otherParams"
          :key="param.code"
          :control="param.control ?? ''"
          :name="param.name ?? param.code ?? ''"
          :config="asObject(param.config)"
          :model-value="data.params[param.code ?? ''] ?? param.defaultValue"
          @update:model-value="editParam(param.code ?? '', $event)"
        />
      </div>
      <span class="node-estimate smartcut-price" aria-hidden="true" />
      <button
        class="node-generate"
        type="button"
        :disabled="Boolean(sendReason)"
        :title="sendReason || '发送：打开剪辑台并按要求剪一版'"
        aria-label="发送"
        data-testid="smartcut-send"
        @click="send"
      ><ArrowUp :size="18" /></button>
    </div>
    <p v-if="sendReason || smartcutConfig.state.value === 'error'" class="smartcut-send-hint" data-testid="smartcut-send-reason">
      <span>{{ sendReason }}</span>
      <button v-if="smartcutConfig.state.value === 'error'" type="button" class="smartcut-send-retry" @click="smartcutConfig.refresh()">重试</button>
      <small v-if="usage" class="smartcut-usage">{{ usage }}</small>
    </p>
    </template>
  </section>
</template>

<style scoped>
.smartcut-top { justify-content: space-between; min-height: 44px; }
.smartcut-ref-anchor { position: relative; }
.smartcut-ref-chip {
  display: inline-flex; align-items: center; gap: 3px; height: 26px; padding: 0 10px; border: 0; border-radius: 999px;
  color: var(--cv-n80, var(--canvas-text)); background: var(--cv-n22, var(--canvas-line)); font-size: 12px; cursor: pointer;
}
.smartcut-ref-chip:hover:not(:disabled) { color: var(--cv-n96, var(--canvas-text)); }
.smartcut-ref-chip:disabled { opacity: 0.5; cursor: not-allowed; }
.smartcut-expand { display: grid; place-items: center; width: 28px; height: 28px; border: 0; border-radius: 6px; color: var(--cv-n70, var(--canvas-muted)); background: transparent; cursor: pointer; }
.smartcut-expand:hover { color: var(--cv-n96, var(--canvas-text)); background: var(--cv-n22, var(--canvas-line)); }
.smartcut-ref-menu {
  position: absolute; top: calc(100% + 4px); left: 0; z-index: 30; width: 240px; max-height: 240px; margin: 0; padding: 4px; overflow-y: auto;
  border: 1px solid var(--canvas-line); border-radius: 8px; background: var(--canvas-panel, var(--portal-elevated)); list-style: none;
  box-shadow: 0 8px 24px var(--portal-shadow);
}
.smartcut-ref-entries { display: grid; gap: 2px; width: 180px; }
.smartcut-ref-entries :deep(.smartcut-ref-entry), .smartcut-ref-entry {
  width: 100%; height: 32px; display: flex; align-items: center; justify-content: flex-start; gap: 8px; padding: 0 10px;
  border: 0; border-radius: 5px; color: var(--cv-n90, var(--canvas-text)); background: transparent; font-size: 12px; cursor: pointer;
}
.smartcut-ref-entries :deep(.smartcut-ref-entry:hover), .smartcut-ref-entry:hover { background: var(--canvas-line); }
.smartcut-ref-menu li { display: flex; justify-content: space-between; gap: 8px; padding: 6px 8px; border-radius: 5px; color: var(--cv-n90, var(--canvas-text)); font-size: 12px; cursor: pointer; }
.smartcut-ref-menu li:hover, .smartcut-ref-menu li:focus-visible { background: var(--canvas-line); outline: none; }
.smartcut-ref-menu li.empty { color: var(--canvas-muted); cursor: default; }
.smartcut-ref-menu li.hint { color: var(--portal-gold); cursor: default; }
.smartcut-ref-menu li.hint:hover { background: transparent; }
.smartcut-ref-menu small { color: var(--canvas-muted); }
.smartcut-inputs { padding: 6px 16px 0; }
.smartcut-price { color: var(--cv-n80); }
.smartcut-send-hint {
  display: flex; align-items: center; gap: 8px; margin: 0; padding: 0 16px 10px; color: var(--canvas-muted); font-size: 11px;
}
.smartcut-send-hint > span { min-width: 0; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.smartcut-send-retry { flex: 0 0 auto; border: 1px solid var(--canvas-line); border-radius: 5px; padding: 1px 7px; color: var(--cv-n80); background: transparent; font-size: 10px; cursor: pointer; }
.smartcut-usage { flex: 0 0 auto; color: var(--canvas-muted); font-size: 10px; }
</style>
