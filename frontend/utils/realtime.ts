/**
 * 实时事件的 topic 与生成事件判定 —— 纯函数，便于单测。
 *
 * 事件经宿主的事件通道送来（stores/realtime，dev/01 TTD-3），每条是 `{topic, payload}`。
 * ⚠️ 生成事件按**工作区**投递（`workspace:{id}`），不是按项目 —— 订 `project:{id}` 一条都收不到。
 */

export function workspaceTopic(workspaceId: string): string {
  return `workspace:${workspaceId}`;
}

/**
 * 项目 topic（`project:{id}`）。⚠️ 生成事件**不走**这里（按工作区投递，见文件头）；
 * 它留给画布协作类事件（`canvas.ops_applied` 等）。
 */
export function projectTopic(projectId: string): string {
  return `project:${projectId}`;
}

type Frame = { type?: unknown; generationId?: unknown; projectId?: unknown };

/**
 * 一条生成事件要不要处理、落到哪个节点；不处理返回 null。
 *
 * ⚠️ 事件只当「该去对账了」的信号，**不拿事件里的字段当结果**：产物要另取（文本产物的正文只在产物端点上），
 *    原因与状态以库里为准 —— 事件不是事实来源（GO-26），会乱序、会重复、会丢。
 * ⚠️ 只处理本页**正在跟踪**的任务：
 *  - 已经落定的不再跟踪 —— 晚到的旧事件（乱序）会把一个已完成的节点打回「运行中」；
 *  - 同一工作区里别人 / 别的标签页发起的不处理 —— 每个开着的标签页都去写结果、存画布的话，彼此 409。
 */
export function eventTarget(
  frame: unknown,
  projectId: string,
  tracked: ReadonlyMap<string, string>
): { generationId: string; nodeId: string } | null {
  const f = frame as Frame | null;
  if (f?.type !== "generation.update" || typeof f.generationId !== "string") return null;
  if (f.projectId !== projectId) return null;
  const nodeId = tracked.get(f.generationId);
  return nodeId ? { generationId: f.generationId, nodeId } : null;
}
