<script setup lang="ts">
/**
 * 工具步骤行（设计 03 §3.3）。标题按工具类型（`utils/agentMessages.stepTitle`），图标 ◌ 进行中 / ✓ 完成 /
 * ✕ 失败 / ⊘ 已取消。展开后按工具给细节；生成步骤的产出卡与文档步骤的文件卡**不用展开就显示**。
 *
 * 相邻只读步骤合并成的那一行也由本组件画（`steps` 传多个），展开后逐条列出。
 */
import { Ban, Check, ChevronDown, CircleDot, LoaderCircle, X } from "@lucide/vue";
import { ElMessage } from "element-plus";
import FileCard from "./FileCard.vue";
import OutputCard from "./OutputCard.vue";
import {
  credits,
  mergedStepsTitle,
  stepFile,
  stepIcon,
  stepNodeIds,
  stepOutputs,
  stepTitle,
  type AgentStep
} from "~/utils/agentMessages";
import { AGENT_PANEL_BRIDGE, NOOP_BRIDGE } from "~/utils/agentPanelBridge";

const props = defineProps<{
  steps: AgentStep[];
  args?: Record<string, unknown>;
  readOnlyReason?: string;
  /** 正在等的生成预计耗时（秒），只给生成中的那一步。 */
  estimateSeconds?: number;
}>();
const emit = defineEmits<{ retry: [title: string] }>();
const bridge = inject(AGENT_PANEL_BRIDGE, NOOP_BRIDGE);

const merged = computed(() => props.steps.length > 1);
const step = computed(() => props.steps[0]!);
const args = computed(() => props.args ?? {});
const display = computed(() => (step.value.display ?? {}) as Record<string, unknown>);
const title = computed(() => (merged.value ? { label: mergedStepsTitle(props.steps), detail: "" } : stepTitle(step.value, args.value)));
const icon = computed(() => stepIcon(step.value.status));
const outputs = computed(() => (merged.value ? undefined : stepOutputs(step.value)));
const file = computed(() => (merged.value ? undefined : stepFile(step.value)));
const nodeIds = computed(() => (step.value.tool === "canvas_apply" ? stepNodeIds(step.value) : []));
const params = computed(() => {
  const raw = args.value.params && typeof args.value.params === "object" ? args.value.params as Record<string, unknown> : {};
  const list: string[] = [];
  const model = String(args.value.modelCode ?? display.value.modelCode ?? "");
  if (model) list.push(`模型 ${model}`);
  const duration = args.value.durationSec ?? raw.duration;
  if (duration) list.push(`${String(duration)} 秒`);
  const ratio = raw.ratio ?? raw.aspectRatio;
  if (ratio) list.push(String(ratio));
  if (args.value.count) list.push(`${String(args.value.count)} 个`);
  return list;
});
const expandable = computed(() => merged.value
  || step.value.status === "failed"
  || nodeIds.value.length > 0
  || (step.value.tool === "load_skill" && Boolean(display.value.description))
  || (step.value.tool === "quote" && display.value.available !== undefined));
const expanded = ref(false);

function locate(id: string) {
  if (!bridge.locate([id])) ElMessage.info("这个节点已经不在画布上了");
}

const canvas = useCanvasStore();
/** 节点标题（画布上找不到时退回 ID 末几位）。 */
function canvasTitle(id: string): string {
  const node = bridge.canvasLoaded ? canvas.nodes.find(item => item.id === id) : undefined;
  return node?.data.title || `节点 ${id.slice(-4)}`;
}

function subtitleFor(childStep: AgentStep) {
  const t = stepTitle(childStep);
  return t.detail ? `${t.label} · ${t.detail}` : t.label;
}
</script>

<template>
  <div class="agent-step" :class="[`is-${icon}`, { 'is-expanded': expanded }]" :data-step-id="step.id">
    <button type="button" class="agent-step-head" :disabled="!expandable" :aria-expanded="expanded" @click="expanded = !expanded">
      <span class="agent-step-icon" :title="{ running: '进行中', waiting: '等待中', done: '完成', failed: '失败', cancelled: '已取消' }[icon]">
        <LoaderCircle v-if="icon === 'running'" :size="13" class="spin" />
        <CircleDot v-else-if="icon === 'waiting'" :size="13" />
        <Check v-else-if="icon === 'done'" :size="13" />
        <X v-else-if="icon === 'failed'" :size="13" />
        <Ban v-else :size="13" />
      </span>
      <strong>{{ title.label }}</strong>
      <span class="agent-step-detail">{{ title.detail }}</span>
      <ChevronDown v-if="expandable" :size="13" class="agent-step-chevron" />
    </button>

    <div v-if="expanded" class="agent-step-body">
      <ul v-if="merged" class="agent-step-list">
        <li v-for="child in steps" :key="child.id">{{ subtitleFor(child) }}</li>
      </ul>
      <template v-else>
        <p v-if="step.status === 'failed'" class="agent-error-text">{{ step.errorMessage || '这一步没有成功' }}</p>
        <p v-if="step.tool === 'load_skill' && display.description" class="agent-muted">{{ display.description }}</p>
        <p v-if="step.tool === 'quote'" class="agent-muted">
          {{ display.label || display.modelCode }}<template v-if="display.available === false"> · {{ display.blockedReason || '暂不可用' }}</template><template v-else> · {{ credits(Number(display.credits ?? 0)) }}</template>
        </p>
        <!-- 生成步骤的完整提示词不在这里展示（design/03 R-9）；成功的生成步骤也不能展开（R-10），参数行只在失败等可展开时出现。 -->
        <p v-if="params.length || step.creditsQuoted" class="agent-muted">
          {{ params.join(' · ') }}<template v-if="step.creditsQuoted"> · 费用 {{ credits(step.creditsQuoted) }}</template>
        </p>
        <div v-if="nodeIds.length" class="agent-ref-chips">
          <button v-for="id in nodeIds" :key="id" type="button" class="agent-chip" title="在画布上定位" @click="locate(id)">▶ {{ canvasTitle(id) }}</button>
        </div>
        <button v-if="step.status === 'failed' && !readOnlyReason" type="button" class="agent-link-button" @click="emit('retry', title.label)">重试这一步</button>
      </template>
    </div>

    <OutputCard
      v-if="outputs"
      :outputs="outputs"
      :title="title.detail || title.label"
      :started-at="step.startedAt"
      :estimate-seconds="estimateSeconds"
      :read-only-reason="readOnlyReason"
      @retry="emit('retry', title.label)"
    />
    <FileCard v-if="file" :path="file.path" :doc-id="file.docId" :node-id="file.nodeId" :subtitle="file.created ? '项目文档 · 新建' : '项目文档 · 已更新'" />
  </div>
</template>

