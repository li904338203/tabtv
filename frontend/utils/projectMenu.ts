/**
 * 项目卡片「⋯」菜单里文件夹那一项给什么。
 *
 * ⚠️ 按**项目自己的 folderId** 判，不按页面在哪个文件夹里判：
 *    首页没有「当前文件夹」这个上下文，而全部项目页搜索时会跨文件夹列出项目 ——
 *    按页面判的话，搜出来的、本在文件夹里的项目只给「移动至」，永远移不出来。
 *
 *  - `move-out`：已在某个文件夹里 → 只给「移出文件夹」（再列一遍文件夹、选回它自己是空操作）
 *  - `move-into`：未分类且有文件夹 → 「移动至文件夹 ▸」子菜单
 *  - `none`：未分类且一个文件夹都没有 → 不出现这一项（出现一个点开是空的子菜单更糟）
 */
export type FolderMenuMode = "move-out" | "move-into" | "none";

export function folderMenuMode(folderId: string | null | undefined, folderCount: number): FolderMenuMode {
  if (folderId) return "move-out";
  return folderCount > 0 ? "move-into" : "none";
}
