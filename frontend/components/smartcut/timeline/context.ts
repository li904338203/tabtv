import type { InjectionKey } from "vue";

/**
 * 时间线里各片段块共用的「取受保护图片」：拼图与图片缩略图要 fetch 成 blob 再给 object URL，
 * 由 TimelineView 在自己的组件作用域里持有（卸载即释放），片段块经 inject 取用，不各自持有一份。
 * 返回空串 = 还没取到（画占位）。
 */
export type SmartcutTimelineMedia = {
  sheetUrl: (mediaId: string) => string;
  imageUrl: (mediaId: string) => string;
};

export const smartcutTimelineMediaKey: InjectionKey<SmartcutTimelineMedia> = Symbol("smartcut-timeline-media");

/** 一个要画在轨道上的条目（片段、字幕、文字层），几何已换算成像素。 */
export type SmartcutLaneItem = {
  id: string;
  kind: "clip" | "subtitle" | "text";
  left: number;
  width: number;
  startMs: number;
  durationMs: number;
  label: string;
  /** 帧级时码等补充说明。 */
  meta: string;
  title: string;
  mediaId?: string;
  mediaKind?: "video" | "image" | "audio";
  sourceInMs?: number;
  sourceOutMs?: number;
  speed?: number;
  /** 画面轨有转场时与上一段重叠的像素宽度（画接缝用）。 */
  overlapPx?: number;
  marks: { ai?: boolean; disconnected?: boolean; generating?: boolean; failed?: boolean; transition?: boolean; hidden?: boolean; anchored?: boolean };
  /** 最近一次片段级生成失败的原因（片段上的「失败」标记悬停显示，SCT-40）。 */
  failReason?: string;
  /** 这一段正在生成时的进度（0~100，取自 generation.update；还没收到进度时为 0），不在生成时不给。 */
  progress?: number;
  /** 能不能拖两端裁切（片段）；字幕 / 文字层只整体平移。 */
  trimmable: boolean;
};
