// @tabtv/ui —— 仅共享设计 token 与**无业务**组件（08 §1）。
// 禁止在此放置业务页面状态：用户端与管理端共享基础类型，但不共享业务状态。

/** 任务状态，与 api/openapi.yaml 的 GenerationStatus 一致。 */
export type TaskStatus =
  | 'queued'
  | 'running'
  | 'success'
  | 'failed'
  | 'cancelled'

/**
 * 任务状态 → CSS 变量名。
 * ⚠️ cancelled 映射到中性色而非危险色：用户主动取消不是故障。
 */
export const taskStatusVar: Record<TaskStatus, string> = {
  queued: 'var(--task-queued)',
  running: 'var(--task-running)',
  success: 'var(--task-success)',
  failed: 'var(--task-failed)',
  cancelled: 'var(--task-cancelled)',
}

/** 轮询耗尽时的本地态。**不是**服务端终态，不得渲染成 failed。 */
export const taskUnknownVar = 'var(--task-unknown)'

/** 缩略图宽度档位。开放任意值等于给人一个把 CPU 打满的 URL 参数。 */
export const THUMBNAIL_WIDTHS = [160, 320, 640] as const
export type ThumbnailWidth = (typeof THUMBNAIL_WIDTHS)[number]
