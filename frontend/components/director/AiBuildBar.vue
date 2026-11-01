<script setup lang="ts">
/**
 * 编辑器里的 AI 搭景条（DRT-20，dev/03 §8）：描述 → 发送（免费）→ 等待 → 预览提案（图例 + 默认值说明）→ 应用 / 放弃。
 *
 * - 参考图：入边上游（被选为全景的那一条除外）→ 节点上上传 / `@` 引用的参考，去重后取前 N 张，多出的说出来（DR-F-100）；
 * - 置灰给最根本的理由：只读 → 服务端说不可用（附原因）→ 提案预览中 → 输入问题（DR-AC-32）；
 * - 失败按业务码给文案，不匹配 message；40902 转进冲突流程（`directorBuildErrorText`）；
 * - 「不再等待」**不会让服务端停下**：那次调用照常跑完、照常计入每小时次数 —— 文案不写「已取消」（DRT-20 风险 2）；
 * - 应用只走 `store.applyProposal()`（一次 commit + 正常保存，DRD-26），放弃只清提案、场景版本不变（DR-AC-27）。
 */
import { ElMessage } from "element-plus";
import { LoaderCircle, Sparkles, X } from "@lucide/vue";
import { DirectorFlushError } from "~/stores/director";
import type { UploadedReference } from "~/utils/canvas";
import { directorBuildDisabledReason, directorBuildErrorText, directorBuildReferences } from "~/utils/directorBuild";
import type { DirectorUpstreamImage } from "~/utils/directorCanvas";
import { DIRECTOR_POSES } from "~/utils/directorCatalog";

const props = defineProps<{
  defaultPrompt: string;
  uploads: UploadedReference[];
  upstreamImages: DirectorUpstreamImage[];
  /** 从画布节点面板点「发送」打开的：载入完成后自动发起一次（只发一次）。 */
  autoStart: boolean;
}>();
const emit = defineEmits<{ close: [] }>();
const store = useDirectorStore();
const workspace = useWorkspaceStore();
const build = useDirectorBuildConfig();

const KNOWN_POSES = new Set(DIRECTOR_POSES.map(pose => pose.code));
const prompt = ref(props.defaultPrompt);
const waiting = ref(false);
const failure = ref("");
let waitToken = 0;

const panoramaSource = computed(() => {
  const background = store.scene?.background as { mode?: string; panorama?: { sourceNodeId?: string } } | undefined;
  return background?.mode === "panorama" ? background.panorama?.sourceNodeId ?? null : null;
});
const refs = computed(() => directorBuildReferences({
  incoming: props.upstreamImages,
  panoramaSourceNodeId: panoramaSource.value,
  uploads: props.uploads,
  maxImages: build.config.value?.maxImages ?? 0
}));
const disabledReason = computed(() => directorBuildDisabledReason({
  config: build.config.value,
  configError: build.state.value === "error" ? build.error.value : undefined,
  role: workspace.current?.role,
  prompt: prompt.value,
  refs: props.uploads.length,
  proposalOpen: Boolean(store.proposal)
}));
const usage = computed(() => (build.config.value ? `本小时已用 ${build.config.value.usedThisHour} / ${build.config.value.perHour} 次` : ""));
const proposal = computed(() => store.proposal);
const unknownPoses = computed(() => [...new Set((proposal.value?.scene.objects ?? [])
  .map(object => String((object.character as { posePreset?: string } | undefined)?.posePreset ?? ""))
  .filter(code => code && !KNOWN_POSES.has(code)))]);

async function send() {
  if (waiting.value) return;
  if (disabledReason.value) {
    failure.value = disabledReason.value;
    return;
  }
  const token = ++waitToken;
  waiting.value = true;
  failure.value = "";
  const mediaIds = refs.value.mediaIds;
  const dropped = refs.value.dropped;
  try {
    const result = await store.requestBuild({ prompt: prompt.value.trim(), mediaIds });
    if (token !== waitToken) return;
    if (!store.setProposal(result)) {
      failure.value = "场景当前不能预览提案，请稍后再试";
      return;
    }
    if (dropped > 0) ElMessage.info(`参考图最多 ${build.config.value?.maxImages ?? mediaIds.length} 张，多出的 ${dropped} 张没有带上`);
  } catch (cause) {
    if (token !== waitToken) return;
    if (cause instanceof DirectorFlushError) {
      failure.value = `没有发起搭景：${cause.message}`;
    } else {
      const text = directorBuildErrorText(cause);
      if (text.conflict) store.handleUploadConflict(cause);
      failure.value = text.text;
    }
  } finally {
    if (token === waitToken) waiting.value = false;
    void build.refresh();
  }
}

/** 不再等待：只是界面不等了，服务端照常把那次调用跑完、照常计次。 */
function stopWaiting() {
  waitToken++;
  waiting.value = false;
  failure.value = "已不再等待这次搭景（这次调用仍会跑完并计入本小时次数）";
}

/** 应用 / 放弃是用户自己结束的提案；其余情况下提案消失（远端更新使它作废）要说出来。 */
let endingByUser = false;

async function apply() {
  endingByUser = true;
  failure.value = "";
  try {
    if (await store.applyProposal()) ElMessage.success("已应用 AI 搭景（可以撤销）");
  } catch (cause) {
    failure.value = cause instanceof Error && cause.message ? cause.message : "应用失败";
  } finally {
    endingByUser = false;
  }
}

function discard() {
  endingByUser = true;
  store.discardProposal();
  endingByUser = false;
}

watch(() => store.proposal, (now, before) => {
  if (before && !now && !endingByUser) failure.value = "场景刚被修改，这份 AI 方案已作废，请重新搭景";
}, { flush: "sync" });

onMounted(async () => {
  await build.refresh();
  if (props.autoStart) await send();
});
</script>

<template>
  <section class="ai-build-bar" aria-label="AI 搭景" data-testid="ai-build-bar">
    <header>
      <strong><Sparkles :size="14" />AI 搭景</strong>
      <span class="ai-free">免费</span>
      <small v-if="usage">{{ usage }}</small>
      <!-- 有提案时不能收起：应用 / 放弃都在条里，收起了视口就一直锁着 -->
      <button v-if="!proposal" type="button" class="ai-close" title="收起" aria-label="收起 AI 搭景" @click="emit('close')"><X :size="14" /></button>
    </header>

    <template v-if="proposal">
      <ul class="ai-legend" aria-label="图例">
        <li><i class="added" />新增 {{ proposal.changes.added.length }}</li>
        <li><i class="updated" />修改 {{ proposal.changes.updated.length }}</li>
        <li><i class="removed" />删除 {{ proposal.changes.removed.length }}</li>
      </ul>
      <ul v-if="proposal.defaults.length || unknownPoses.length" class="ai-defaults" aria-label="默认值与说明">
        <li v-for="(line, index) in proposal.defaults" :key="index">{{ line }}</li>
        <li v-for="code in unknownPoses" :key="code">姿势 {{ code }} 暂不支持，先按站姿显示</li>
      </ul>
      <p class="dr-hint">预览期间场景锁定：应用后可以撤销，放弃不改动场景。</p>
      <div class="dr-seg">
        <button type="button" class="dr-btn primary" @click="apply">应用</button>
        <button type="button" class="dr-btn" @click="discard">放弃</button>
      </div>
    </template>

    <template v-else-if="waiting">
      <p class="ai-waiting" role="status"><LoaderCircle :size="14" class="spin" />AI 正在搭景（免费）</p>
      <button type="button" class="dr-btn" @click="stopWaiting">不再等待</button>
    </template>

    <template v-else>
      <textarea
        v-model="prompt"
        class="dr-input ai-prompt"
        rows="3"
        :maxlength="build.config.value?.maxPromptChars ?? 2000"
        placeholder="描述想要搭建的场景，支持通过参考图创建"
        aria-label="描述想要搭建的场景"
      />
      <p class="dr-hint">
        参考图 {{ refs.mediaIds.length }} 张<template v-if="refs.dropped">，超出上限的 {{ refs.dropped }} 张不会带上</template>
        <template v-if="refs.pending.length">；{{ refs.pending.join("、") }} 还没有产出，没有算进去</template>
      </p>
      <p v-if="failure" class="ai-failure" role="alert">{{ failure }}</p>
      <div class="dr-seg">
        <button type="button" class="dr-btn primary" :disabled="Boolean(disabledReason)" :title="disabledReason || '发送（免费）'" @click="send">
          <Sparkles :size="13" />发送
        </button>
        <span v-if="disabledReason" class="dr-hint">{{ disabledReason }}</span>
        <button v-if="build.state.value === 'error'" type="button" class="dr-btn" @click="build.refresh()">重试</button>
      </div>
    </template>
    <p v-if="failure && (waiting || proposal)" class="ai-failure" role="alert">{{ failure }}</p>
  </section>
</template>

<style scoped>
.ai-build-bar {
  position: absolute;
  left: 12px;
  top: 64px;
  z-index: 8;
  display: grid;
  gap: 8px;
  width: min(420px, calc(100% - 24px));
  padding: 10px 12px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 8px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 8px 24px var(--portal-shadow);
  font-size: 12px;
}
.ai-build-bar header { display: flex; align-items: center; gap: 8px; }
.ai-build-bar header strong { display: inline-flex; align-items: center; gap: 5px; }
.ai-build-bar header small { color: var(--portal-muted); }
.ai-free { padding: 0 6px; border-radius: 999px; color: var(--portal-on-green); background: var(--portal-green); font-size: 11px; }
.ai-close { display: grid; place-items: center; width: 24px; height: 24px; margin-left: auto; padding: 0; border: 0; border-radius: 5px; color: var(--portal-text-soft); background: transparent; cursor: pointer; }
.ai-close:hover { background: var(--portal-hover); }
.ai-prompt { height: auto; min-height: 64px; padding: 6px 8px; line-height: 1.5; resize: vertical; }
.ai-legend { display: flex; gap: 12px; margin: 0; padding: 0; list-style: none; }
.ai-legend li { display: flex; align-items: center; gap: 5px; }
.ai-legend i { width: 10px; height: 10px; border-radius: 2px; }
.ai-legend i.added { background: #22c55e; }
.ai-legend i.updated { background: #f59e0b; }
.ai-legend i.removed { background: #ef4444; opacity: 0.6; }
.ai-defaults { display: grid; gap: 3px; max-height: 120px; margin: 0; padding: 0 0 0 16px; overflow-y: auto; color: var(--portal-text-soft); }
.ai-waiting { display: flex; align-items: center; gap: 6px; margin: 0; }
.ai-failure { margin: 0; color: var(--portal-danger); }
.spin { animation: ai-spin 0.9s linear infinite; }
@keyframes ai-spin { to { transform: rotate(360deg); } }
@media (prefers-reduced-motion: reduce) { .spin { animation: none; } }
</style>
