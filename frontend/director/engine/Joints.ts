/**
 * 关节模式的控制点（DRT-21，DR-F-43，dev/03 §9.3）：选中角色、打开关节模式后，每个可调关节一个小球。
 *
 * - 小球放 layer 1（辅助层）：视口里看得到、截图里没有（DR-AC-15）；用辅助专用色（品红，选中的关节青色）；
 *   不做深度测试，被身体挡住的关节也看得见、点得着；
 * - 拾取 ID 是 `joint:<骨骼名>`（`DIRECTOR_JOINT_PICK_PREFIX`），关节模式下拾取只看这些小球；
 * - 位置每帧跟着骨骼走：拖关节、解 IK、移骨盆时骨骼一直在动；
 * - `handle`：拖手脚末端（IK）与骨盆（移动身体）时，变换手柄挂的是这个空物体（放在骨骼的世界位置），
 *   不直接挂骨骼 —— 平移骨骼本身会把肢体拉长，而场景里只存旋转；
 * - 几何与两种材质只建一份，关闭编辑器时由 Disposer 释放。
 */
import { Group, Mesh, MeshBasicMaterial, Object3D, SphereGeometry, Vector3, type Bone, type SkinnedMesh } from "three";
import { DIRECTOR_JOINT_BONES } from "~/utils/directorPose";
import type { EngineContext } from "./context";
import { HELPER_CYAN, HELPER_MAGENTA, LAYER_HELPERS } from "./layers";

export const DIRECTOR_JOINT_PICK_PREFIX = "joint:";

/** 小球半径占身高的比例（1.75 m 的人约 2.5 cm）。 */
const RADIUS_RATIO = 0.0145;
const _v = new Vector3();

export class Joints {
  /** IK 末端与骨盆拖动时变换手柄挂的空物体（世界坐标，挂在场景根上）。 */
  readonly handle = new Object3D();
  private readonly group = new Group();
  private readonly spheres = new Map<string, Mesh>();
  private readonly geometry: SphereGeometry;
  private readonly normal: MeshBasicMaterial;
  private readonly active: MeshBasicMaterial;
  private skinned: SkinnedMesh | null = null;
  private characterId: string | null = null;
  private selected: string | null = null;

  constructor(private readonly ctx: EngineContext) {
    this.geometry = ctx.disposer.geometry(new SphereGeometry(1, 16, 12));
    const material = (color: number) => ctx.disposer.material(new MeshBasicMaterial({ color, depthTest: false, depthWrite: false, transparent: true, opacity: 0.9 }));
    this.normal = material(HELPER_MAGENTA);
    this.active = material(HELPER_CYAN);
    this.group.name = "joints";
    this.group.layers.set(LAYER_HELPERS);
    this.handle.name = "joint-handle";
    this.handle.layers.set(LAYER_HELPERS);
    ctx.scene.add(this.group, this.handle);
  }

  /** 关节模式打开在哪个角色上（null = 关着）。 */
  get id(): string | null {
    return this.characterId;
  }

  get selectedBone(): string | null {
    return this.selected;
  }

  /**
   * 打开（`characterId` 非空）/ 关闭关节模式。素模还没加载时 `skinned` 为 null：先记下角色，小球等下一次 `show` 再建。
   * 换了素模（换模型会换一份克隆）也要重新 `show`。
   */
  show(characterId: string | null, skinned: SkinnedMesh | null, height: number): void {
    if (characterId !== this.characterId) this.selected = null;
    this.characterId = characterId;
    if (skinned === this.skinned && characterId) {
      this.update();
      return;
    }
    this.clearSpheres();
    this.skinned = characterId ? skinned : null;
    if (!this.skinned) return;
    const radius = Math.max(0.012, height * RADIUS_RATIO);
    for (const { bone } of DIRECTOR_JOINT_BONES) {
      if (!this.skinned.skeleton.getBoneByName(bone)) continue;
      const sphere = new Mesh(this.geometry, this.normal);
      sphere.name = `joint:${bone}`;
      sphere.scale.setScalar(radius);
      sphere.renderOrder = 20;
      sphere.layers.set(LAYER_HELPERS);
      sphere.userData.directorPickId = `${DIRECTOR_JOINT_PICK_PREFIX}${bone}`;
      this.spheres.set(bone, sphere);
      this.group.add(sphere);
    }
    this.paint();
    this.update();
  }

  bone(name: string | null | undefined): Bone | undefined {
    return name ? this.skinned?.skeleton.getBoneByName(name) : undefined;
  }

  select(bone: string | null): void {
    this.selected = bone && this.spheres.has(bone) ? bone : null;
    this.paint();
    this.ctx.invalidate();
  }

  /** 小球跟到骨骼当前的世界位置（每帧渲染前调一次；骨骼的世界矩阵先刷新）。 */
  update(): void {
    const skinned = this.skinned;
    if (!skinned || !this.spheres.size) return;
    skinned.skeleton.bones[0]?.updateWorldMatrix(true, true);
    for (const [name, sphere] of this.spheres) {
      const bone = skinned.skeleton.getBoneByName(name);
      if (bone) sphere.position.copy(bone.getWorldPosition(_v));
    }
  }

  /** 把手柄空物体放到某根骨骼的世界位置（开始拖 IK / 骨盆之前）。 */
  placeHandle(bone: Bone): void {
    bone.updateWorldMatrix(true, false);
    this.handle.position.copy(bone.getWorldPosition(_v));
    this.handle.quaternion.identity();
    this.handle.updateMatrixWorld(true);
  }

  pickTargets(): Mesh[] {
    return [...this.spheres.values()];
  }

  /** 关节小球的个数（调试与检查脚本用）。 */
  get count(): number {
    return this.spheres.size;
  }

  private paint(): void {
    for (const [name, sphere] of this.spheres) sphere.material = name === this.selected ? this.active : this.normal;
  }

  private clearSpheres(): void {
    // 只摘掉节点：几何与材质是共享的，留给 Disposer 在关闭时释放
    this.group.clear();
    this.spheres.clear();
  }
}
