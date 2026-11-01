/**
 * 素材广场（风格 / 特效 / 运镜）的纯逻辑：文案、查询参数、空状态、节点上选中的运镜。
 * 弹窗本身在 `components/canvas/MaterialPlaza.vue`。
 *
 * ⚠️ 新后端的列表是**裸数组**（`GET /canvas/{styles,effects,cameras}`），筛选参数是 `keyword` / `favorited`，
 *    收藏是 `POST /canvas/materials/{type}/{code}/favorite`、按 **code** 认。老前端的素材库读的是
 *    `{list,total}`、`kw` / `favorite`、按 id 收藏 —— 对着新后端一条都读不出来，列表恒空、选了也不生效。
 */

export type MaterialPlazaType = "style" | "effect" | "camera";

export const MATERIAL_PLAZA_TEXT: Record<MaterialPlazaType, { title: string; search: string; noun: string; path: string }> = {
  style: { title: "风格广场", search: "搜索风格名称", noun: "风格", path: "styles" },
  effect: { title: "特效广场", search: "搜索特效名称", noun: "特效", path: "effects" },
  camera: { title: "运镜广场", search: "搜索运镜名称", noun: "运镜", path: "cameras" }
};

/** 列表的查询参数：空关键词不带（`?keyword=` 在后端是「按空串筛」）。 */
export function materialPlazaQuery(keyword: string, favoritesOnly: boolean): Record<string, string> {
  const query: Record<string, string> = {};
  const text = keyword.trim();
  if (text) query.keyword = text;
  if (favoritesOnly) query.favorited = "true";
  return query;
}

/** 空状态说什么：「还没收藏」「搜不到」「目录是空的」是三件事，不能都说「暂无内容」。 */
export function materialPlazaEmptyText(type: MaterialPlazaType, state: { keyword: string; favorites: boolean }): string {
  const noun = MATERIAL_PLAZA_TEXT[type].noun;
  const keyword = state.keyword.trim();
  if (keyword) return `没有找到「${keyword}」`;
  if (state.favorites) return `还没有收藏的${noun}：悬停卡片，点右上角的星标即可收藏`;
  return type === "camera"
    ? "运镜广场还没有内容"
    : `${MATERIAL_PLAZA_TEXT[type].title}还没有内容，可在后台「风格与特效」里添加`;
}

/** 封面的列表缩略图（受保护媒体，按 320px 取：原图一张就有几 MB，广场一屏十几张）。 */
export function materialPlazaCoverUrl(coverUrl: string | undefined | null): string | undefined {
  if (!coverUrl) return undefined;
  return coverUrl.includes("?") ? coverUrl : `${coverUrl}?w=320`;
}

/** 节点上选中的运镜（`params.camera`）：只存 code 与显示名，提示词由服务端按目录拼。 */
export type CanvasCameraMove = { code: string; name: string };

/**
 * 读出节点上选中的运镜。认对象 `{code,name}` 与只有 code 的字符串；
 * ⚠️ 此前短暂用过「镜头运动 / 景别 / 机位」三组选项的对象（没有 code），那种按没选处理。
 */
export function canvasCameraMove(value: unknown): CanvasCameraMove | undefined {
  if (typeof value === "string") {
    const code = value.trim();
    return code ? { code, name: code } : undefined;
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const raw = value as Record<string, unknown>;
  const code = typeof raw.code === "string" ? raw.code.trim() : "";
  if (!code) return undefined;
  const name = typeof raw.name === "string" && raw.name.trim() ? raw.name.trim() : code;
  return { code, name };
}
