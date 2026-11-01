/**
 * AI 搭景提案的预览标记（DRT-20，DR-F-102）：新增的对象绿框、修改的琥珀框、被删除的对象在原位置留 30% 透明的红色虚影。
 *
 * - 标记都在 layer 1（辅助层）：视口里看得到，截图（只画 layer 0）里没有；预览期间截图本来也是禁用的；
 * - 几何与材质只建一份（单位立方体 + 三种颜色），每次 `show` 只是增删场景节点、按包围盒摆放缩放 ——
 *   反复预览不会一直申请新的显存；它们登记在 Disposer 里，编辑器关闭时一起释放；
 * - 框线关掉深度测试：被别的对象挡住的修改也看得出来。
 */
import { BoxGeometry, EdgesGeometry, Group, LineBasicMaterial, LineSegments, Mesh, MeshBasicMaterial, Vector3, type Box3, type Object3D, type Scene } from "three";
import type { Disposer } from "./Disposer";
import { LAYER_HELPERS } from "./layers";

/** 三种标记色（与界面图例一致）。⚠️ 不能用辅助专用的品红 / 青（截图检查靠它们认辅助元素），也不用手柄的纯红绿蓝。 */
export const DIRECTOR_PROPOSAL_COLORS = { added: 0x22c55e, updated: 0xf59e0b, removed: 0xef4444 } as const;

export type DirectorProposalBoxes = { added: Box3[]; updated: Box3[]; removed: Box3[] };

const MIN_SIZE = 0.05;

export class ProposalOverlay {
  private readonly group = new Group();
  private readonly edges: EdgesGeometry;
  private readonly cube: BoxGeometry;
  private readonly lineMaterials: Record<"added" | "updated", LineBasicMaterial>;
  private readonly ghostMaterial: MeshBasicMaterial;

  constructor(scene: Scene, disposer: Disposer) {
    this.cube = disposer.geometry(new BoxGeometry(1, 1, 1));
    this.edges = disposer.geometry(new EdgesGeometry(this.cube));
    this.lineMaterials = {
      added: disposer.material(new LineBasicMaterial({ color: DIRECTOR_PROPOSAL_COLORS.added, depthTest: false, transparent: true })),
      updated: disposer.material(new LineBasicMaterial({ color: DIRECTOR_PROPOSAL_COLORS.updated, depthTest: false, transparent: true }))
    };
    this.ghostMaterial = disposer.material(new MeshBasicMaterial({
      color: DIRECTOR_PROPOSAL_COLORS.removed, transparent: true, opacity: 0.3, depthWrite: false
    }));
    this.group.name = "proposal-overlay";
    this.group.layers.set(LAYER_HELPERS);
    scene.add(this.group);
  }

  show(boxes: DirectorProposalBoxes): void {
    this.clear();
    for (const box of boxes.added) this.group.add(this.place(new LineSegments(this.edges, this.lineMaterials.added), box));
    for (const box of boxes.updated) this.group.add(this.place(new LineSegments(this.edges, this.lineMaterials.updated), box));
    for (const box of boxes.removed) this.group.add(this.place(new Mesh(this.cube, this.ghostMaterial), box));
  }

  clear(): void {
    // 只摘掉节点：几何与材质是共享的，留给 Disposer 在关闭时释放
    this.group.clear();
  }

  get count(): number {
    return this.group.children.length;
  }

  private place<T extends Object3D>(object: T, box: Box3): T {
    box.getCenter(object.position);
    const size = box.getSize(new Vector3());
    object.scale.set(Math.max(size.x, MIN_SIZE), Math.max(size.y, MIN_SIZE), Math.max(size.z, MIN_SIZE));
    object.layers.set(LAYER_HELPERS);
    object.renderOrder = 10;
    return object;
  }
}
