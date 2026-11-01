import { existingOutputGeneration, resourceNodePositions, type ResourceMediaType } from "~/utils/canvasResources";

/**
 * 「添加资源」（上传 / 从生成历史选择）建节点：每份一个「已有产出」节点（只带媒体 ID，GAP-16），一次撤销撤掉整批；
 * 建好当场取预览（`restore()` 只在页面挂载时跑过）。⚠️ `updateGeneration` 不排保存，最后必须 `scheduleSave`。
 *
 * 只给按需加载的组件用（CanvasAddResources / GenerationHistoryPicker / 视频节点截帧 nodes/VideoNodeBody）：放进 CanvasBoard 的话这段进画布页首屏包，
 * 而首屏包体积有上限（scripts/director-bundle-check.mjs）。返回建好的节点 ID，选中与对焦由画布做。
 */
export function useCanvasResourceNodes() {
  const canvas = useCanvasStore();
  const generation = useGenerationStore();
  /**
   * 文本资产（`type: "text"`）建成手写文本节点（内容在 params.text，没有模型 —— 整图运行把它当静态输入）；
   * 媒体资产建成「已有产出」节点。
   * `options.position` 是第一个节点的落点（视频截帧放在视频节点右侧）；不给就用画布的默认位置。其余在它右边排开。
   */
  async function addResourceNodes(
    resources: Array<{ type: ResourceMediaType | "text"; mediaId?: string; text?: string; title: string }>,
    options: { position?: { x: number; y: number } } = {}
  ): Promise<string[]> {
    if (!resources.length) return [];
    canvas.snapshot();
    let positions: Array<{ x: number; y: number }> = [];
    const created = resources.map((resource, index) => {
      const params = resource.type === "text" ? { textMode: "manual", text: resource.text ?? "" } : {};
      const position = index ? positions[index] : options.position;
      const id = canvas.addNode(resource.type, resource.title, params, {
        ...(position ? { position } : {}), exactTitle: true, recordHistory: false, persist: false
      });
      if (!index) positions = resourceNodePositions(canvas.nodes.find(node => node.id === id)?.position ?? { x: 480, y: 220 }, resources.length);
      if (resource.type !== "text" && resource.mediaId) canvas.updateGeneration(id, existingOutputGeneration(resource.type, resource.mediaId));
      return id;
    });
    canvas.scheduleSave();
    await nextTick();
    for (const id of created) {
      const node = canvas.nodes.find(item => item.id === id);
      if (node && node.data.nodeType !== "text") void generation.restoreNode(node).catch(() => undefined);
    }
    return created;
  }
  return { addResourceNodes };
}
