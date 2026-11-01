/**
 * 图层与辅助专用色（DRD-39，DR-F-44 / 64，dev/03 §14）。
 *
 * - layer 0：画面内容（角色、道具、地面、全景、灯光）。截图、预览小窗只看这一层。
 * - layer 1：编辑辅助（网格、坐标轴、机位模型与视锥、选中框、变换手柄、名称标签）。导演相机与机位视角看 0 + 1。
 * - layer 2：拾取代理（角色的包围盒）。任何相机都不看它，只有拾取射线看。
 *
 * ⚠️ 辅助一律放 layer 1、且要 `traverse` 到每个子物体：漏一层子物体，它就会出现在截图里（DR-AC-15）。
 * ⚠️ 自绘辅助只用品红 / 青两种专用色：截图检查靠「这两种颜色出现 0 次」判定辅助没有漏进画面，
 *    画面内容（素模蓝灰、灰阶道具、地面）里不会出现这两种纯色。
 */
import type { Object3D } from "three";

export const LAYER_CONTENT = 0;
export const LAYER_HELPERS = 1;
export const LAYER_PICK = 2;

/** 辅助专用色：机位模型、视锥、X 轴。 */
export const HELPER_MAGENTA = 0xff00ff;
/** 辅助专用色：网格、Z 轴、选中框。 */
export const HELPER_CYAN = 0x00ffff;

/** 把一棵子树整体放到某一层（只在这一层，不再出现在 layer 0）。 */
export function directorSetLayer(root: Object3D, layer: number): void {
  root.traverse((o) => o.layers.set(layer));
}
