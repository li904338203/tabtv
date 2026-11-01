/**
 * 素模角色（DR-F-40 / 41 / 42，dev/03 §4.2 Characters、§9，R-3 #2 / #4 / #5）。
 *
 * 结构：`root`（transform：脚底位置、朝向、等比缩放，stage 局部坐标）→ `heightNode`（身高 ÷ 静止身高的等比缩放）
 * → `offsetNode`（套完姿势后把最低点落回地面的位移）→ 素模克隆（`SkeletonUtils.clone`）。
 * 另有两个 root 的子物体：拾取代理（layer 2，DRT-13）与名称标签（layer 1）。
 *
 * - 骨骼名在构建时已统一成标准名（R-3 #2），直接 `getBoneByName(标准名)`；
 * - 姿势：`bone.quaternion = rest × q(Euler(x°, y°, z°, "XYZ"))`；`boneOverrides` 与预设同一根骨骼都有值时**取覆盖值**、
 *   不叠加（R-3 #4）；
 * - 落地：姿势只有旋转，套完之后按**蒙皮后的最低点**把模型落回脚底锚点；腾空姿势再抬 `lift × 身高`（R-3 #5）；
 * - 身高 = 静止包围盒高度等比缩放到 `character.height`；体型 = 三个 morph 的整组权重（`directorBodyMorphWeights`）；
 * - 材质全场共享一份；几何与 GLB 缓存共享（克隆不复制几何），关闭时释放 GPU 副本，CPU 端的解析结果留在缓存里。
 *
 * ⚠️ 蒙皮网格的射线检测按绑定姿势算，不按当前姿势：坐着的人要点到「站着时的位置」才能选中。所以拾取用代理盒，
 *    代理盒按**摆好姿势之后**的包围盒摆放（躺倒、坐下时与站姿差得很多）。
 * ⚠️ 套姿势后的包围球会变，three 按旧的包围球做视锥剔除会把人剔掉：素模一律 `frustumCulled = false`。
 */
import {
  Box3,
  BoxGeometry,
  Euler,
  Group,
  Matrix4,
  Mesh,
  MeshBasicMaterial,
  Quaternion,
  Vector3,
  type Material,
  type Object3D,
  type SkinnedMesh
} from "three";
import type { GLTF } from "three/addons/loaders/GLTFLoader.js";
import type { CSS2DObject } from "three/addons/renderers/CSS2DRenderer.js";
import { clone as cloneSkinned } from "three/addons/utils/SkeletonUtils.js";
import { DIRECTOR_OBJECT_DEFAULTS, directorModelOf, type DirectorBodyCode, type DirectorModelCode } from "~/utils/directorCatalog";
import { DIRECTOR_MORPH_TARGETS, directorBodyMorphWeights, type DirectorBoneRotation } from "~/utils/directorRig";
import { DIRECTOR_SAMPLED_LIFT } from "~/utils/directorAnimation";
import { directorUniformScale, type DirectorObject } from "~/utils/directorScene";
import type { DirectorPoseLookup } from "~/utils/directorTimeline";
import { directorLoadMannequin, directorLoadPoses, directorPoseFor, type DirectorPoseData } from "../assets";
import { asObject, bool, DEG, num, str, vec3, type EngineContext } from "./context";
import { LAYER_PICK } from "./layers";
import type { LabelLayer } from "./LabelLayer";

/** 名称标签离头顶的距离（米）。 */
const LABEL_GAP = 0.12;

type Mannequin = {
  readonly code: DirectorModelCode;
  readonly clone: Object3D;
  readonly skinned: SkinnedMesh;
  /** 每根骨骼的 rest 朝向（与 `skeleton.bones` 同序）。 */
  readonly rest: Quaternion[];
  /** 静止身高（模型单位）。 */
  readonly restHeight: number;
};

export type CharacterEntry = {
  readonly id: string;
  readonly root: Group;
  readonly heightNode: Group;
  readonly offsetNode: Group;
  /** 拾取代理（layer 2）：按摆好姿势后的包围盒摆放。 */
  readonly proxy: Mesh;
  readonly label: CSS2DObject;
  /** 摆好姿势后的包围盒（root 局部坐标）。 */
  readonly box: Box3;
  mannequin: Mannequin | null;
  object: DirectorObject;
  signature: string;
  shapeKey: string;
  token: number;
};

/** 每个素模的静止身高（模型单位，rest 姿势 + 标准体型），按 GLB 缓存。 */
const restHeights = new WeakMap<GLTF, number>();

const _m = new Matrix4();
const _v = new Vector3();
const _euler = new Euler();
const _q = new Quaternion();

/**
 * 蒙皮 + morph 之后、在 `space` 局部坐标里的包围盒。
 *
 * ⚠️ 子树要用 `updateMatrixWorld` 刷新，不能用 `updateWorldMatrix(…, true)`：只有前者会刷新蒙皮网格的
 *    `bindMatrixInverse`（attached 模式），`getVertexPosition` 靠它把蒙皮结果换回网格局部坐标。用旧值的话，
 *    角色不在原点时包围盒会带上它的世界位移 —— 实测「悬空」角色的代理盒与标签偏到了旁边 1.5 m 处，
 *    落地位移还把 root 的 0.3 m 高度抵消掉了；在原点的角色看不出来。
 */
export function skinnedBox(skinned: SkinnedMesh, space: Object3D, out: Box3): Box3 {
  space.updateWorldMatrix(true, false);
  space.updateMatrixWorld(true);
  _m.copy(space.matrixWorld).invert().multiply(skinned.matrixWorld);
  out.makeEmpty();
  const count = skinned.geometry.getAttribute("position").count;
  for (let i = 0; i < count; i++) out.expandByPoint(skinned.getVertexPosition(i, _v).applyMatrix4(_m));
  return out;
}

/** 找到素模里的蒙皮网格（群众烘焙也用，DRT-22）。 */
export function findSkinned(root: Object3D): SkinnedMesh | null {
  let found: SkinnedMesh | null = null;
  root.traverse((o) => {
    if (!found && (o as SkinnedMesh).isSkinnedMesh) found = o as SkinnedMesh;
  });
  return found;
}

export class Characters {
  private readonly entries = new Map<string, CharacterEntry>();
  private readonly pending = new Set<Promise<unknown>>();
  private readonly proxyGeometry: BoxGeometry;
  private readonly proxyMaterial: MeshBasicMaterial;
  private material: Material | null = null;
  private poses: Map<string, DirectorPoseData> | null = null;
  private posesLoading: Promise<void> | null = null;
  private disposed = false;

  constructor(
    private readonly ctx: EngineContext,
    private readonly labels: LabelLayer
  ) {
    this.proxyGeometry = ctx.disposer.geometry(new BoxGeometry(1, 1, 1));
    this.proxyMaterial = ctx.disposer.material(new MeshBasicMaterial({ visible: false }));
    ctx.disposer.defer(() => {
      this.disposed = true;
    });
  }

  get(id: string | null | undefined): CharacterEntry | undefined {
    return id ? this.entries.get(id) : undefined;
  }

  ids(): string[] {
    return [...this.entries.keys()];
  }

  /** 在途的加载（素模、姿势库）全部结束（成功或失败）后 resolve。 */
  async whenIdle(): Promise<void> {
    while (this.pending.size) await Promise.allSettled([...this.pending]);
  }

  upsert(object: DirectorObject): CharacterEntry {
    let entry = this.entries.get(object.id);
    if (!entry) entry = this.create(object);
    const signature = JSON.stringify(object);
    if (entry.signature === signature) return entry;
    entry.signature = signature;
    entry.object = object;

    const t = asObject(object.transform);
    const p = vec3(t.position, { x: 0, y: 0, z: 0 });
    const r = vec3(t.rotation, { x: 0, y: 0, z: 0 });
    // 角色只能等比缩放（DR-F-33）：旧数据里三轴不等的统一成等比，下一次拖动结束时随 transform-end 写回。
    const s = directorUniformScale(vec3(t.scale, { x: 1, y: 1, z: 1 })).x;
    entry.root.position.set(p.x, p.y, p.z);
    entry.root.rotation.set(r.x * DEG, r.y * DEG, r.z * DEG, "XYZ");
    entry.root.scale.setScalar(s);
    entry.root.visible = bool(object.visible, true);
    this.labels.setText(entry.label, str(object.name, ""));

    const c = asObject(object.character);
    const shapeKey = JSON.stringify([c.model, c.height, c.body, c.posePreset, c.boneOverrides ?? null, c[DIRECTOR_SAMPLED_LIFT] ?? null]);
    if (shapeKey !== entry.shapeKey) {
      entry.shapeKey = shapeKey;
      this.reshape(entry);
    }
    this.ctx.invalidate();
    return entry;
  }

  remove(id: string): void {
    const entry = this.entries.get(id);
    if (!entry) return;
    this.entries.delete(id);
    entry.token++;
    this.labels.release(entry.label);
    if (entry.mannequin) this.detachModel(entry.mannequin);
    entry.root.removeFromParent();
    this.ctx.invalidate();
  }

  /** 姿势库查询（时间轴的姿势插值用，DRT-24）：认不出的码按自然站立，与套姿势同一个回落（`directorPoseFor`）。 */
  poseLookup(): DirectorPoseLookup {
    return (code) => {
      const pose = directorPoseFor(this.poses, code);
      return pose ? { bones: pose.bones, ...(pose.lift !== undefined ? { lift: pose.lift } : {}) } : undefined;
    };
  }

  /** 角色的蒙皮网格（素模还没加载时为 null）：关节模式、IK 与骨骼读数用（DRT-21）。 */
  skinnedOf(id: string | null | undefined): SkinnedMesh | null {
    return this.get(id)?.mannequin?.skinned ?? null;
  }

  /** 某根骨骼的 rest 朝向：骨骼覆盖按「相对 rest」存（DRD-4）。取不到时为 null。 */
  restOf(id: string, bone: string): Quaternion | null {
    const m = this.get(id)?.mannequin;
    if (!m) return null;
    const index = m.skinned.skeleton.bones.findIndex((b) => b.name === bone);
    return index >= 0 ? (m.rest[index] as Quaternion) : null;
  }

  /** 角色的目标身高（米，未乘 root 缩放）。 */
  heightOf(id: string): number {
    const entry = this.get(id);
    return entry ? this.targetHeight(entry) : 1.7;
  }

  /** 拾取目标：可见、未锁定的角色的代理盒（DRT-13）。 */
  pickTargets(): Mesh[] {
    const out: Mesh[] = [];
    for (const entry of this.entries.values()) {
      if (entry.object.locked === true || entry.object.visible === false) continue;
      out.push(entry.proxy);
    }
    return out;
  }

  private create(object: DirectorObject): CharacterEntry {
    const root = new Group();
    root.name = `character:${object.id}`;
    const heightNode = new Group();
    const offsetNode = new Group();
    heightNode.add(offsetNode);
    const proxy = new Mesh(this.proxyGeometry, this.proxyMaterial);
    proxy.userData.directorPickId = object.id;
    proxy.layers.set(LAYER_PICK);
    root.add(heightNode, proxy);
    const label = this.labels.create(root, str(object.name, ""));
    this.ctx.stage.add(root);
    const entry: CharacterEntry = {
      id: object.id,
      root,
      heightNode,
      offsetNode,
      proxy,
      label,
      box: new Box3(),
      mannequin: null,
      object,
      signature: "",
      shapeKey: "",
      token: 0
    };
    this.entries.set(object.id, entry);
    return entry;
  }

  /** 素模、身高、体型、姿势变了：必要时换模型，再整体重摆。 */
  private reshape(entry: CharacterEntry): void {
    const c = asObject(entry.object.character);
    const code = (directorModelOf(str(c.model, ""))?.code ?? DIRECTOR_OBJECT_DEFAULTS.character.model) as DirectorModelCode;
    const token = ++entry.token;
    if (!entry.mannequin) this.placeholder(entry);
    const needModel = entry.mannequin?.code !== code;
    const needPoses = !this.poses;
    if (!needModel && !needPoses) {
      this.pose(entry);
      return;
    }
    const job = Promise.all([needModel ? directorLoadMannequin(code) : null, this.loadPoses()]).then(([gltf]) => {
      if (this.disposed || token !== entry.token || this.entries.get(entry.id) !== entry) return;
      if (gltf) this.attachModel(entry, code, gltf);
      this.pose(entry);
      this.ctx.invalidate();
    });
    this.track(
      job.catch((err) => {
        console.error(`[director] 素模 ${code} 加载失败`, err);
      })
    );
  }

  private loadPoses(): Promise<void> {
    if (this.poses) return Promise.resolve();
    if (!this.posesLoading) {
      this.posesLoading = directorLoadPoses().then(
        (poses) => {
          this.poses = poses;
        },
        (err) => {
          // 姿势库取不到时按「无预设」摆（rest + 覆盖），不让整个角色出不来；下次打开编辑器再试。
          console.error("[director] 姿势库加载失败", err);
          this.poses = new Map();
        }
      );
    }
    return this.posesLoading;
  }

  private track(p: Promise<unknown>): void {
    this.pending.add(p);
    void p.finally(() => this.pending.delete(p));
  }

  private attachModel(entry: CharacterEntry, code: DirectorModelCode, gltf: GLTF): void {
    const source = findSkinned(gltf.scene);
    if (!source) throw new Error(`素模 ${code} 里没有蒙皮网格`);
    if (!this.material) this.material = this.ctx.disposer.material((source.material as Material).clone());
    if (entry.mannequin) this.detachModel(entry.mannequin);
    const clone = cloneSkinned(gltf.scene);
    const skinned = findSkinned(clone);
    if (!skinned) throw new Error(`素模 ${code} 克隆失败`);
    skinned.material = this.material;
    skinned.castShadow = true;
    skinned.receiveShadow = true;
    skinned.frustumCulled = false;
    this.ctx.disposer.geometry(skinned.geometry);
    this.ctx.disposer.skeleton(skinned.skeleton);
    entry.offsetNode.position.set(0, 0, 0);
    entry.offsetNode.add(clone);
    let restHeight = restHeights.get(gltf);
    if (restHeight === undefined) {
      skinned.morphTargetInfluences?.fill(0);
      const box = skinnedBox(skinned, entry.offsetNode, new Box3());
      restHeight = Math.max(0.1, box.max.y - box.min.y);
      restHeights.set(gltf, restHeight);
    }
    entry.mannequin = { code, clone, skinned, rest: skinned.skeleton.bones.map((b) => b.quaternion.clone()), restHeight };
  }

  /** 摘掉一个素模克隆：几何与材质是共享的（关闭时统一释放），骨骼纹理每个克隆一份，立刻释放。 */
  private detachModel(m: Mannequin): void {
    m.clone.removeFromParent();
    this.ctx.disposer.releaseSkeleton(m.skinned.skeleton);
  }

  /** 素模还没加载出来时：按身高给一个人形大小的代理盒，先能选中、能拖。 */
  private placeholder(entry: CharacterEntry): void {
    const height = this.targetHeight(entry);
    entry.box.min.set(-0.25, 0, -0.15);
    entry.box.max.set(0.25, height, 0.15);
    this.fitProxy(entry);
  }

  private targetHeight(entry: CharacterEntry): number {
    const c = asObject(entry.object.character);
    const model = directorModelOf(str(c.model, "")) ?? directorModelOf(DIRECTOR_OBJECT_DEFAULTS.character.model);
    return Math.max(0.1, num(c.height, model?.heightM ?? 1.7));
  }

  /** 体型、姿势、身高、落地，再更新代理盒与标签。 */
  private pose(entry: CharacterEntry): void {
    const m = entry.mannequin;
    if (!m) return;
    const c = asObject(entry.object.character);
    const body = str(c.body, DIRECTOR_OBJECT_DEFAULTS.character.body) as DirectorBodyCode;
    const weights = directorBodyMorphWeights(m.code, ["slim", "standard", "stocky"].includes(body) ? body : "standard");
    const dict = m.skinned.morphTargetDictionary ?? {};
    const influences = m.skinned.morphTargetInfluences;
    if (influences) {
      for (const name of DIRECTOR_MORPH_TARGETS) {
        const i = dict[name];
        if (i !== undefined) influences[i] = weights[name];
      }
    }

    const preset = directorPoseFor(this.poses, str(c.posePreset, ""));
    const overrides = asObject(c.boneOverrides) as Record<string, DirectorBoneRotation>;
    // 同一根骨骼：覆盖值取代预设值（R-3 #4），所以是对象展开而不是逐个相乘。
    const bones: Record<string, DirectorBoneRotation> = { ...(preset?.bones ?? {}), ...overrides };
    const skeleton = m.skinned.skeleton;
    skeleton.bones.forEach((bone, i) => bone.quaternion.copy(m.rest[i] as Quaternion));
    for (const [name, r] of Object.entries(bones)) {
      const bone = skeleton.getBoneByName(name);
      const v = asObject(r);
      if (!bone || typeof v.x !== "number" || typeof v.y !== "number" || typeof v.z !== "number") continue;
      if (!Number.isFinite(v.x) || !Number.isFinite(v.y) || !Number.isFinite(v.z)) continue;
      const index = skeleton.bones.indexOf(bone);
      _euler.set(v.x * DEG, v.y * DEG, v.z * DEG, "XYZ");
      bone.quaternion.copy(m.rest[index] as Quaternion).multiply(_q.setFromEuler(_euler));
    }

    const restHeight = m.restHeight;
    const scale = this.targetHeight(entry) / restHeight;
    entry.heightNode.scale.setScalar(scale);
    entry.offsetNode.position.set(0, 0, 0);
    const box = skinnedBox(m.skinned, entry.offsetNode, entry.box);
    // 时间轴在两个姿势之间插出来的离地比例（只在投影对象上，DRT-24）优先；否则取预设自己的
    const sampledLift = Number(c[DIRECTOR_SAMPLED_LIFT]);
    const lift = Number.isFinite(sampledLift) ? Math.max(0, sampledLift) : preset?.lift && preset.lift > 0 ? preset.lift : 0;
    const shift = -box.min.y + lift * restHeight;
    entry.offsetNode.position.y = shift;
    // 换到 root 局部坐标：先抬 shift，再乘身高缩放。
    box.min.y += shift;
    box.max.y += shift;
    box.min.multiplyScalar(scale);
    box.max.multiplyScalar(scale);
    this.fitProxy(entry);
  }

  private fitProxy(entry: CharacterEntry): void {
    const box = entry.box;
    box.getCenter(entry.proxy.position);
    box.getSize(entry.proxy.scale);
    entry.proxy.scale.max(_v.set(0.05, 0.05, 0.05));
    entry.label.position.set(entry.proxy.position.x, box.max.y + LABEL_GAP, entry.proxy.position.z);
  }
}
