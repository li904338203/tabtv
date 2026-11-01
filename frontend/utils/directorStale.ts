/**
 * 截图是否过期（DRT-17，dev/03 §7.3）：截图记录上的渲染指纹 ≠ 场景当前的指纹即为过期。
 *
 * ⚠️ **前端不算指纹**（DRD-13），只比较服务端给的值：在前端拿浮点重算一遍，会出现「一保存就过期」（DRR-14）。
 *    当前指纹来自项目场景摘要（`useDirectorScenes`），记录上的指纹是截图时镜像进节点 params 的。
 * ⚠️ 拿不准时不报过期（没有摘要、没有指纹）：误报的提示比漏报更伤人 —— 用户会去重新截图、再花一次下游的钱。
 */
import type { JsonObject } from "~/types/canvas";
import type { DirectorSceneSummary } from "./directorCanvas";
import { directorCaptureSummaries, type DirectorSourceRef } from "./directorPublish";

type SummaryLike = Pick<DirectorSceneSummary, "renderFingerprint" | "outputCameraId">;

export function directorIsStale(
  fingerprint: string | null | undefined,
  summary: Pick<SummaryLike, "renderFingerprint"> | null | undefined
): boolean {
  if (!fingerprint || !summary?.renderFingerprint) return false;
  return fingerprint !== summary.renderFingerprint;
}

/**
 * 导演台节点是否过期：**只看输出机位**那一条截图 —— 下游拿到的就是它；别的机位的截图旧了不影响下游。
 * 输出机位换成了一个还没截过图的机位、而节点上还挂着旧的输出截图时，也算过期（下游拿到的是旧机位的画面）。
 */
export function directorNodeStale(params: JsonObject, summary: SummaryLike | null | undefined): boolean {
  if (!summary?.outputCameraId) return false;
  const entry = directorCaptureSummaries(params).find(item => item.cameraId === summary.outputCameraId);
  if (!entry) return /^\d+$/.test(String(params.outputMediaId ?? ""));
  return directorIsStale(entry.renderFingerprint, summary);
}

/** 发布节点是否过期：节点上那条记录的指纹 ≠ 来源场景当前的指纹。 */
export function directorPublishedStale(
  source: Pick<DirectorSourceRef, "renderFingerprint"> | null | undefined,
  summary: Pick<SummaryLike, "renderFingerprint"> | null | undefined
): boolean {
  return Boolean(source) && directorIsStale(source?.renderFingerprint, summary);
}

export type DirectorStaleNotice = {
  text: string;
  /** 给用户的出路：重新截图 / 改用输出机位；来源已删除时没有出路。 */
  action: "recapture" | "use_output" | null;
};

/**
 * 过期提示的文案与出路（DR-F-83、DR-F-84、DR-UX-15），优先级从高到低：
 * 来源导演台已删除（只说明）→ 机位已删除（改用输出机位）→ 过期（重新截图）。都不成立时为 null。
 * `cameraExists` 只有加载了场景才知道（编辑器里），不知道时按「还在」处理。
 */
export function directorStaleText(input: { stale: boolean; cameraExists?: boolean; sourceDeleted?: boolean; preview?: boolean }): DirectorStaleNotice | null {
  if (input.sourceDeleted) return { text: "来源导演台已删除", action: null };
  if (input.cameraExists === false) return { text: "该机位已删除", action: "use_output" };
  // 预演是一段视频（DRT-25）：「重新截图」会把它换成一张图，所以不给这个动作，只说去导演台重新导出
  if (input.stale && input.preview) return { text: "场景已更新，预演可能过期：回到导演台重新导出", action: null };
  if (input.stale) return { text: "场景已更新，截图可能过期", action: "recapture" };
  return null;
}
