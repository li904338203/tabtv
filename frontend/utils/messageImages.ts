/**
 * 用户消息里要显示的图片（2026-10-25 按用户要求：发出去的消息带图时，气泡里要看得到图，而不只是一个「▶ 裙子」胶囊）。
 *
 * 引用里存的不一定是媒体 ID，按种类换算：
 *   - `marketing_product`：id 是**商品库的 ID**。商品图在建会话时冻结进 `sceneInput.products`（MKD-25：下游只读快照、
 *     不回查商品库），所以显示快照里的全部商品图（按顺序，第 1 张为主图）；
 *   - `marketing_character`：id 是角色库的 ID。快照里的角色是 `kind=media`、`id`=主图，并带 `characterId`，按它对上；
 *   - `media`：id 就是媒体 ID。只显示图片（`mediaKind=image`；旧数据没写类型的也试一下，取不到就是占位）。
 * 画布节点、资产、源视频这些仍然只是胶囊。
 */
import type { components } from "@tabtv/api-client";

type SceneInput = components["schemas"]["AgentSceneInput"];

export type MessageImage = { key: string; mediaId: string; label: string };

/** 气泡里最多直接显示几张（多出来的在最后一张上标「+N」，点开预览能翻到）。 */
export const MESSAGE_IMAGE_LIMIT = 8;

export function userMessageImages(refs: unknown, scene?: Pick<SceneInput, "products" | "characters"> | null): MessageImage[] {
  const out: MessageImage[] = [];
  const seen = new Set<string>();
  const push = (mediaId: string | undefined, label: string) => {
    if (!mediaId || seen.has(mediaId)) return;
    seen.add(mediaId);
    out.push({ key: mediaId, mediaId, label });
  };
  for (const raw of Array.isArray(refs) ? refs : []) {
    const ref = raw && typeof raw === "object" ? raw as Record<string, unknown> : {};
    const kind = typeof ref.kind === "string" ? ref.kind : "";
    const id = typeof ref.id === "string" ? ref.id : "";
    const label = typeof ref.label === "string" ? ref.label.trim() : "";
    if (kind === "marketing_product") {
      (scene?.products ?? []).forEach((mediaId, i) => push(mediaId, `${label || "商品"} · 第 ${i + 1} 张`));
    } else if (kind === "marketing_character") {
      const character = (scene?.characters ?? []).find(c => c.kind === "media" && c.characterId === id);
      push(character?.id, label || character?.name || "角色");
    } else if (kind === "media") {
      const mediaKind = typeof ref.mediaKind === "string" ? ref.mediaKind : "";
      if (mediaKind !== "" && mediaKind !== "image") continue;
      push(id, label || (ref.slot === "product" ? "商品图" : ref.slot === "character" ? "角色" : "素材"));
    }
  }
  return out;
}
