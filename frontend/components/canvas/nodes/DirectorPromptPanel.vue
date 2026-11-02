<script setup lang="ts">
/**
 * 导演台节点的提示词面板（DRT-20，DR-F-04）：选中导演台节点时挂在卡片下方，代替通用参数面板。
 *
 * - 描述写进 `params.prompt`，参考图写进 `params.reference`。`+` 上传与 `@` 画布节点是同一个形状，
 *   `@` 来的多带一个 `fromNodeId`（DR-D-05）。两者都是节点参数，走画布正常的自动保存；
 * - 「发送」**不在这里发请求**：校验通过后打开编辑器并自动搭景一次（`pendingAction: build`）——
 *   提案要在 3D 视口里预览、确认之后才落进场景（DR-F-102）；
 * - 「本小时已用 N / M 次」来自 build-config（DRK-4），置灰理由与编辑器里的 AI 条用同一个函数。
 *   tabtv 不计价（TT-T-14）：原「免费」价格角标删除，次数说明留在原位置。
 */
import { ArrowUp, AtSign, Sparkles } from "@lucide/vue";
import { ElMessage } from "element-plus";
import MentionPicker from "~/components/agent/composer/MentionPicker.vue";
import type { CanvasNodeData, JsonObject } from "~/types/canvas";
import { referenceResources } from "~/utils/canvas";
import { directorBuildDisabledReason } from "~/utils/directorBuild";
import { directorAddMention, directorMentionCandidates } from "~/utils/directorCanvas";
import { directorCanEdit } from "~/utils/directorRole";
import ReferenceResourceToken from "../ReferenceResourceToken.vue";
import ReferenceUploadButton from "../ReferenceUploadButton.vue";

const props = defineProps<{ nodeId: string; data: CanvasNodeData }>();
const emit = defineEmits<{ send: [] }>();
const canvas = useCanvasStore();
const configStore = useCanvasConfigStore();
const workspace = useWorkspaceStore();
const build = useDirectorBuildConfig();

const mentionOpen = ref(false);

function configOf(code: string): JsonObject {
  const param = configStore.nodeType("director")?.params?.find(item => item.code === code);
  const config = param?.config;
  return config && typeof config === "object" && !Array.isArray(config) ? config as JsonObject : {};
}
const referenceConfig = computed(() => configOf("reference"));
const placeholder = computed(() => String(configOf("prompt").placeholder ?? "").trim() || "描述想要搭建的场景，支持通过参考图创建");
/** 与上传按钮同一个口径：没配 maxCount 时按 1 张。 */
const maxCount = computed(() => {
  const value = Number(referenceConfig.value.maxCount ?? 1);
  return Number.isFinite(value) && value >= 1 ? Math.floor(value) : 1;
});
const canEdit = computed(() => directorCanEdit(workspace.current?.role));
const prompt = computed(() => String(props.data.params.prompt ?? ""));
const references = computed(() => referenceResources(props.data.params.reference));
const candidates = computed(() => directorMentionCandidates(canvas.nodes, props.nodeId));
const mentionNodes = computed(() => candidates.value.map(item => ({ id: item.nodeId, label: item.title, thumbMediaId: item.mediaId, hint: "image" })));
const usage = computed(() => (build.config.value ? `本小时已用 ${build.config.value.usedThisHour} / ${build.config.value.perHour} 次` : ""));
const disabledReason = computed(() => directorBuildDisabledReason({
  config: build.config.value,
  configError: build.state.value === "error" ? build.error.value : undefined,
  role: workspace.current?.role,
  prompt: prompt.value,
  refs: references.value.length,
  proposalOpen: false
}));

function editPrompt(value: string) {
  canvas.markTouched(props.nodeId);
  canvas.updateParams(props.nodeId, { ...props.data.params, prompt: value });
}

/** 参考图清空时删掉这个键，不留一个空数组（同 ParamPanel 的上传参考）。 */
function setReferences(value: unknown) {
  canvas.markTouched(props.nodeId);
  const references = referenceResources(value);
  const next: JsonObject = { ...props.data.params };
  if (references.length) next.reference = references;
  else delete next.reference;
  canvas.updateParams(props.nodeId, next);
}

function removeReference(id: string) {
  setReferences(references.value.filter(item => item.id !== id));
}

function pickMention(ref: { id: string }) {
  mentionOpen.value = false;
  const candidate = candidates.value.find(item => item.nodeId === ref.id);
  if (!candidate) return;
  const result = directorAddMention(props.data.params.reference, candidate, maxCount.value);
  if (result.notice) {
    ElMessage.warning(result.notice);
    return;
  }
  setReferences(result.references);
}

function send() {
  if (disabledReason.value) {
    ElMessage.warning(disabledReason.value);
    return;
  }
  emit("send");
}

onMounted(() => {
  void build.load();
});
</script>

<template>
  <section class="node-param-panel director-prompt-panel nodrag nowheel" aria-label="AI 搭景" data-testid="director-prompt-panel" @keydown.esc="mentionOpen = false">
    <div class="node-top-controls">
      <ReferenceUploadButton
        v-if="canEdit"
        :project-id="canvas.projectId"
        :node-id="nodeId"
        :model-value="data.params.reference"
        :config="referenceConfig"
        name="参考图"
        @update:model-value="setReferences"
      />
      <div class="director-mention-anchor">
        <button
          type="button"
          class="canvas-control control-button"
          :disabled="!canEdit"
          :title="canEdit ? '引用画布上已有产出的图片' : '需要编辑权限：你在该工作区是只读成员'"
          aria-label="引用画布上的图片"
          :aria-expanded="mentionOpen"
          @click="mentionOpen = !mentionOpen"
        ><AtSign :size="14" /><span>引用</span></button>
        <MentionPicker
          v-if="mentionOpen"
          class="picker-bottom"
          only="node"
          :nodes="mentionNodes"
          @select="pickMention"
          @close="mentionOpen = false"
        />
      </div>
    </div>
    <div v-if="references.length" class="node-reference-strip" aria-label="参考图">
      <ReferenceResourceToken
        v-for="(reference, index) in references"
        :key="reference.id"
        :value="reference"
        :order="index + 1"
        @remove="removeReference(reference.id)"
      />
    </div>
    <textarea
      :value="prompt"
      :placeholder="placeholder"
      :readonly="!canEdit"
      :maxlength="build.config.value?.maxPromptChars ?? undefined"
      aria-label="描述想要搭建的场景"
      @input="editPrompt(($event.target as HTMLTextAreaElement).value)"
    />
    <div class="node-bottom-controls">
      <span class="director-build-hint">
        <Sparkles :size="13" />AI 搭景
        <small v-if="disabledReason" class="director-build-reason">{{ disabledReason }}</small>
        <button v-if="build.state.value === 'error'" type="button" class="director-build-retry" @click="build.refresh()">重试</button>
      </span>
      <span class="node-estimate director-build-price" :title="usage || undefined"><small v-if="usage">{{ usage }}</small></span>
      <button
        class="node-generate"
        type="button"
        :disabled="Boolean(disabledReason)"
        :title="disabledReason || '发送：打开导演台并按描述搭景'"
        aria-label="发送"
        @click="send"
      ><ArrowUp :size="18" /></button>
    </div>
  </section>
</template>

<style scoped>
.director-mention-anchor { position: relative; }
.director-mention-anchor > button:disabled { opacity: 0.5; cursor: not-allowed; }
.director-mention-anchor :deep(.mention-picker) { width: 260px; right: auto; }
.director-build-hint { min-width: 0; display: inline-flex; align-items: center; gap: 5px; color: var(--cv-n70); font-size: 11px; }
.director-build-reason { min-width: 0; overflow: hidden; color: var(--canvas-muted); font-size: 10px; text-overflow: ellipsis; white-space: nowrap; }
.director-build-retry { flex: 0 0 auto; border: 1px solid var(--canvas-line); border-radius: 5px; padding: 1px 7px; color: var(--cv-n80); background: transparent; font-size: 10px; cursor: pointer; }
.director-build-price { gap: 6px; color: var(--cv-n80); }
.director-build-price small { color: var(--canvas-muted); }
</style>
