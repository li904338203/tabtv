/**
 * 剪辑台里的「去连音频节点 / 去连画面 / 去补镜」（SC-UX-09，SCT-31）：收起剪辑台、选中节点，并让节点下方的指令面板
 * 打开「+ 参考 → 从画布选」。
 *
 * 剪辑台（全屏层）与指令面板（节点下方）不在一棵组件树上，经一份共享状态传话：剪辑台 `request`，面板挂载或收到变化时
 * `consume`（只认自己的节点，取走就清掉 —— 不清的话下次选中这个节点又会自己弹开）。
 */
export type SmartcutLocateKind = "audio" | "picture";
export type SmartcutLocateRequest = { nodeId: string; kind: SmartcutLocateKind; seq: number };

export function useSmartcutLocate() {
  const pending = useState<SmartcutLocateRequest | null>("smartcut-locate", () => null);

  function request(nodeId: string, kind: SmartcutLocateKind) {
    pending.value = { nodeId, kind, seq: (pending.value?.seq ?? 0) + 1 };
  }

  function consume(nodeId: string): SmartcutLocateRequest | null {
    const current = pending.value;
    if (!current || current.nodeId !== nodeId) return null;
    pending.value = null;
    return current;
  }

  return { pending, request, consume };
}
