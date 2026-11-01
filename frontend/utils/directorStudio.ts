/**
 * 3D 导演台编辑器界面的纯函数（DRT-15）：保存状态的文案与样式、按钮置灰的理由。
 * 界面只照着它显示，规则可单测 —— 同一种状态在顶栏与提示条上说两种话，用户会以为遇到了两个问题。
 */
import type { DirectorReadonlyReason, DirectorSaveState } from "~/stores/director";
import type { DirectorVec3 } from "./directorScene";

export type DirectorSaveStateView = {
  label: string;
  /** `quiet` 平常 · `busy` 进行中 · `alert` 醒目（点开提示条）。 */
  tone: "quiet" | "busy" | "alert";
  /** 点一下展开提示条（冲突、离线、失败才有出路可选）。 */
  actionable: boolean;
};

const VIEWS: Record<DirectorSaveState, DirectorSaveStateView> = {
  idle: { label: "已保存", tone: "quiet", actionable: false },
  dirty: { label: "未保存", tone: "quiet", actionable: false },
  saving: { label: "保存中", tone: "busy", actionable: false },
  error: { label: "保存失败", tone: "alert", actionable: true },
  conflict: { label: "有冲突", tone: "alert", actionable: true },
  offline: { label: "离线", tone: "alert", actionable: true }
};

/** 顶栏上的保存状态（DR-UX-08，六种）。 */
export function directorSaveStateView(state: DirectorSaveState): DirectorSaveStateView {
  return VIEWS[state];
}

/** 只读的原因说给用户听（DR-F-156）：不是「看不到按钮」，而是「点不动并知道为什么」。 */
export function directorReadonlyText(reason: DirectorReadonlyReason): string {
  if (reason === "newer") return "这个场景是用更新版本的编辑器保存的，请刷新页面后再编辑";
  if (reason === "forbidden") return "你没有编辑这个场景的权限";
  if (reason === "role") return "你在该工作区是只读";
  return "";
}

/**
 * 撤销 / 重做按钮为什么点不了；能点时为空串。最根本的原因排在前面：只读 → 提案预览中 → 没有可撤销的。
 */
export function directorHistoryDisabledReason(input: {
  readonlyReason: DirectorReadonlyReason;
  proposal: boolean;
  available: boolean;
  kind: "undo" | "redo";
}): string {
  if (input.readonlyReason) return directorReadonlyText(input.readonlyReason);
  if (input.proposal) return "请先应用或放弃 AI 提案";
  if (!input.available) return input.kind === "undo" ? "没有可以撤销的操作" : "没有可以重做的操作";
  return "";
}

/**
 * 「返回画布」能不能直接关：要先把场景存上（DR-F-154）。冲突、离线、保存失败时不关，展开提示条。
 * 返回 null 表示可以关（调用方还要 await `flush()` 成功）。
 */
export function directorCloseBlocker(state: DirectorSaveState): string | null {
  if (state === "conflict") return "场景有冲突，请先在提示条里选一个出路";
  if (state === "offline") return "已离线，恢复网络、改动存上之后再返回";
  if (state === "error") return "改动还没存上，请先重试保存";
  return null;
}

/**
 * 场景树「添加」新对象时的落点（DR-F-11、DR-F-26），由外壳从引擎取：
 * 角色、道具放在视口中心看向的地面点（stage 局部坐标，打不中时为 null → 放原点）；机位用当前导演视角（世界坐标）。
 */
export type DirectorPlacement = {
  ground: DirectorVec3 | null;
  view: { position: DirectorVec3; rotation: DirectorVec3; scale: DirectorVec3 } | null;
};
