import { ElMessage, ElMessageBox } from "element-plus";
import { ErrorCode } from "@tabtv/api-client";
import type { components } from "@tabtv/api-client";
import { SmartcutFlushError } from "~/stores/smartcut";
import { errorCode } from "~/utils/asyncState";
import { smartcutSendDisabledReason } from "~/utils/smartcutCanvas";
import {
  smartcutPlanErrorText,
  smartcutPlanRequestOf,
  smartcutReplaceResult,
  smartcutReplanConfirmText,
  smartcutUserEditedCount,
  type SmartcutPlanFailure,
  type SmartcutUnsuitableAction
} from "~/utils/smartcutPlan";
import { useSmartcutConfig } from "./useSmartcutConfig";
import { useSmartcutLocate } from "./useSmartcutLocate";

type PlanRequest = Omit<components["schemas"]["SmartcutPlanRequest"], "baseVersion">;

/**
 * 剪辑台里的 AI 剪辑交互（docs/agent/smartcut SCT-31，dev/03 §5.5）：剪一版、追加指令、切换方案、仍然应用、
 * 撤销这轮 AI、素材变化的两个处理，以及 42217 / 素材不够的出路。状态与请求都在 `stores/smartcut`，这里只管
 * 「点了之后界面怎么说」：失败按业务码分流（`smartcutPlanErrorText`），被限流时记下多久后再试（面板与剪辑台共用）。
 *
 * ⚠️ 出路只按字段走：42217 看 `details.options`，素材不够看结果里的 `shortage`，不匹配任何文案。
 * ⚠️ 发送前 store 会先 flush：服务端按库里的时间线编译，本地没落盘的改动它看不见（同整图运行「运行前必须先保存」）。
 */
export function useSmartcutAi(opts: {
  /** 剪辑台对应的画布节点（取要求、模式、规格；也是「去连音频节点」要选中的那一个）。 */
  nodeId: () => string;
  /** 剪辑台此刻能不能编辑（只读、屏幕太窄时为 false）。 */
  editable: () => boolean;
  /** 收起剪辑台（去画布上连素材时）。 */
  collapse: () => void | Promise<void>;
  /** 打开识别确认框（「先识别语音」的出路，SCT-36）。 */
  openTranscribe?: (preset: { purpose: "talk" | "narr"; modelCode?: string; keepFillers?: boolean }) => void;
}) {
  const store = useSmartcutStore();
  const canvas = useCanvasStore();
  const smartcutConfig = useSmartcutConfig();
  const locate = useSmartcutLocate();

  /** 结果卡开着（AI 写入、追加指令、方案切换之后弹出，关掉之后「撤销这轮 AI」仍在历史里）。 */
  const resultOpen = ref(false);
  /** 最近一次失败：42217 的出路、限流、冲突（「仍然应用」）与其它错误都在提示条里说。 */
  const failure = ref<SmartcutPlanFailure | null>(null);
  /** 「只替换变了的那几段」的结果（被截短的段要说出来）。 */
  const replaceNotice = ref<{ text: string; details: string[] } | null>(null);

  // 被限流时每秒刷新一次时钟，「请 30 秒后再试」才会倒数；没有被限流时不跑计时器。
  const now = ref(Date.now());
  let ticker: ReturnType<typeof setInterval> | undefined;
  function syncTicker() {
    const limited = smartcutConfig.retryAt.value > Date.now();
    if (limited && !ticker) {
      ticker = setInterval(() => {
        now.value = Date.now();
        if (smartcutConfig.retryAt.value <= now.value) syncTicker();
      }, 1000);
    } else if (!limited && ticker) {
      clearInterval(ticker);
      ticker = undefined;
    }
    now.value = Date.now();
  }
  watch(() => smartcutConfig.retryAt.value, syncTicker, { immediate: true });
  onScopeDispose(() => clearInterval(ticker));

  const retryAfter = computed(() => smartcutConfig.retryAfterSeconds(now.value));
  const readyInputs = computed(() => store.inputs.filter(item => item.status === "ready"
    && (item.kind === "video" || item.kind === "image" || item.kind === "audio" || item.kind === "shotGroup")).length);
  const hasClips = computed(() => (store.timeline?.tracks.find(t => t.kind === "video")?.clips.length ?? 0) > 0);

  /** 剪一版的禁用原因（SC-UX-05）：只读 → 进行中 → 没有素材 → 不可用 → 被限流。 */
  const planDisabledReason = computed(() => smartcutSendDisabledReason({
    canEdit: opts.editable(),
    running: Boolean(store.planRunning),
    readyInputs: readyInputs.value,
    config: smartcutConfig.config.value ?? store.config,
    configError: smartcutConfig.state.value === "error" ? smartcutConfig.error.value : undefined,
    retryAfterSeconds: retryAfter.value
  }));
  /** 追加指令的禁用原因：同上，另外时间线上要先有片段。 */
  const editDisabledReason = computed(() => planDisabledReason.value
    || (hasClips.value ? "" : "时间线上还没有片段，先剪一版再追加要求"));

  function nodeParams(): Record<string, unknown> | undefined {
    const node = canvas.nodes.find(item => item.id === opts.nodeId());
    return node?.data.params as Record<string, unknown> | undefined;
  }

  /** 按节点上的要求、模式与规格组出的请求（发送、按新素材重新剪都用它）。 */
  function nodeRequest(): PlanRequest {
    return smartcutPlanRequestOf(nodeParams());
  }

  async function execute<T>(fn: () => Promise<T>): Promise<T | null> {
    failure.value = null;
    replaceNotice.value = null;
    try {
      const res = await fn();
      void smartcutConfig.refresh();
      return res;
    } catch (cause) {
      if (cause instanceof SmartcutFlushError) {
        ElMessage.warning(cause.message);
        return null;
      }
      const f = smartcutPlanErrorText(cause);
      const code = errorCode(cause);
      if ((code === ErrorCode.RATE_LIMITED || code === ErrorCode.CONCURRENCY_LIMITED) && f.retryAfterSeconds) {
        smartcutConfig.markLimited(f.retryAfterSeconds);
      }
      failure.value = f;
      void smartcutConfig.refresh();
      return null;
    }
  }

  /** 剪一版（SC-F-80）：结果直接写进时间线，弹出结果卡。 */
  async function plan(request: PlanRequest) {
    if (planDisabledReason.value) {
      ElMessage.warning(planDisabledReason.value);
      return null;
    }
    const res = await execute(() => store.runPlan(request));
    if (res) resultOpen.value = true;
    return res;
  }

  /** 在上一次的请求上加开关重剪（42217 与素材不够的出路：允许放慢 / 允许重复 / 不卡点）。 */
  function rerun(extra: Partial<PlanRequest>) {
    const base = store.lastPlanRequest ?? nodeRequest();
    return plan({ ...base, ...extra });
  }

  /** 「按新素材重新剪」（SC-F-151）：用户改过的片段会被替换，先提示（SC-F-84，可撤销）。 */
  async function replan() {
    const text = smartcutReplanConfirmText(smartcutUserEditedCount(store.timeline));
    if (text) {
      try {
        await ElMessageBox.confirm(text, "按新素材重新剪", { confirmButtonText: "重新剪", cancelButtonText: "取消", type: "warning" });
      } catch {
        return null;
      }
    }
    return plan(nodeRequest());
  }

  /** 追加指令（SC-F-110）。 */
  async function edit(instruction: string) {
    if (editDisabledReason.value) {
      ElMessage.warning(editDisabledReason.value);
      return null;
    }
    const res = await execute(() => store.runEdit(instruction));
    if (res) resultOpen.value = true;
    return res;
  }

  /** 切换方案（SC-F-112）：切换即写入那一套。 */
  async function chooseVariant(index: number) {
    if (store.plan?.appliedVariant === index) return null;
    const res = await execute(() => store.applyVariant(index));
    if (res) resultOpen.value = true;
    return res;
  }

  /** 「仍然应用」（SC-F-86）：覆盖剪辑期间的改动，写入前存快照，可撤销。 */
  async function forceApply() {
    const res = await execute(() => store.forceApply());
    if (res) resultOpen.value = true;
    return res;
  }

  /** 不应用：收起「仍然应用」的提示，时间线保持别人改过的样子。 */
  function keepTheirs() {
    store.dismissPlanConflict();
    failure.value = null;
  }

  /** 「只替换变了的那几段」（SC-F-152）：被截短的段逐条说出来。 */
  async function replaceChanged() {
    const res = await execute(() => store.replaceChanged());
    if (!res) return null;
    replaceNotice.value = smartcutReplaceResult(res, store.timeline);
    if (!replaceNotice.value.details.length) ElMessage.success(replaceNotice.value.text);
    return res;
  }

  /**
   * 撤销这轮 AI（SC-F-83）：之后又有手动改动的话先确认 —— 恢复快照会把那些改动一起撤掉（恢复本身也存了快照，还能再回来）。
   */
  async function undoRound() {
    const round = store.aiRound;
    if (!round) {
      ElMessage.info("没有可撤销的 AI 剪辑");
      return false;
    }
    if (round.editedAfter) {
      try {
        await ElMessageBox.confirm("这轮 AI 之后你又改过时间线，撤销会把这些改动一起撤掉（「历史」里还能找回）。", "撤销这轮 AI", {
          confirmButtonText: "仍然撤销", cancelButtonText: "取消", type: "warning"
        });
      } catch {
        return false;
      }
    }
    try {
      const missing = await store.undoAiRound();
      resultOpen.value = false;
      ElMessage.success(missing.length ? `已撤销这轮 AI；有 ${missing.length} 段的素材已被删除，导出前要处理` : "已撤销这轮 AI");
      return true;
    } catch (cause) {
      if (cause instanceof SmartcutFlushError) ElMessage.warning(cause.message);
      else ElMessage.error("撤销失败，时间线保持现状");
      return false;
    }
  }

  async function goConnect(kind: "audio" | "picture") {
    const nodeId = opts.nodeId();
    if (canvas.nodes.some(node => node.id === nodeId)) canvas.selectedNodeId = nodeId;
    locate.request(nodeId, kind);
    await opts.collapse();
  }

  /** 42217 与素材不够的出路（SC-UX-09）。 */
  async function resolveOut(action: SmartcutUnsuitableAction) {
    switch (action) {
      case "allowSlow":
        return rerun({ allowSlow: true });
      case "allowRepeat":
        return rerun({ allowRepeat: true });
      case "noBeat":
        return rerun({ ignoreBeat: true });
      case "connectAudio":
        return goConnect("audio");
      case "connectPicture":
      case "fill":
        // 剪辑台里的补镜（片段级生成）随 SCT-40 上线；在那之前「去补镜」带用户去画布上连更多镜头。
        return goConnect("picture");
      case "transcribe": {
        // 识别确认框的缺省值取自 42217 的报价（服务端按口播 / 解说的要求挑好了模型、口播要保留口癖）。
        const preset = failure.value?.unsuitable?.transcribe ?? {};
        const purpose = (store.lastPlanRequest?.mode ?? nodeRequest().mode) === "narr" ? "narr" : "talk";
        opts.openTranscribe?.({ purpose, ...preset, keepFillers: preset.keepFillers ?? purpose === "talk" });
        return null;
      }
    }
  }

  function dismissFailure() {
    failure.value = null;
  }

  function dismissReplace() {
    replaceNotice.value = null;
  }

  function closeResult() {
    resultOpen.value = false;
  }

  return {
    resultOpen, failure, replaceNotice, retryAfter, planDisabledReason, editDisabledReason,
    nodeRequest, plan, rerun, replan, edit, chooseVariant, forceApply, keepTheirs, replaceChanged, undoRound,
    resolveOut, dismissFailure, dismissReplace, closeResult
  };
}
