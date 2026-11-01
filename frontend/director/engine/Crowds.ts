/**
 * 群众阵列（DR-F-50、DRD-37，dev/03 §4.2 Crowds、§9.4，DRT-22）：一个对象摆出一群人，整阵统一姿势。
 *
 * - **烘焙**：以 `(素模, 姿势)` 为键，克隆素模、套姿势（标准体型），逐顶点做蒙皮变换，得到一份静态几何并重算法线，
 *   缩放到素模的默认身高、底面落到 y = 0（腾空姿势再抬 `lift × 身高`，与单个角色同口径，R-3 #5）。同键复用，
 *   按使用者计数：最后一个使用者删掉或换了姿势时才释放（DR-N-04，反复开关编辑器不泄漏显存）。
 * - **渲染**：一个阵列就是一个 `InstancedMesh`（一次 draw call，DRD-37）；实例按 `directorCrowdLayout` 摆放，
 *   错位由 `seed` 驱动的伪随机数决定，同一种子重开不变（DR-F-50）。
 * - **结构**：`root`（transform：阵列中心的地面点、朝向、等比缩放，stage 局部坐标）→ 实例网格 + 拾取代理。
 * - **拾取**：整阵一个包围盒代理（layer 2，同角色的代理），作为一个对象整体选中、移动；不按 `instanceId` 反查个体。
 *
 * ⚠️ 用 `applyBoneTransform`（three r151 起的名字；以前叫 `boneTransform`）—— 这里经 `SkinnedMesh.getVertexPosition`
 *    调用，它先套 morph 再做蒙皮。锁定的 three 是 0.186.1。
 * ⚠️ 烘焙后**重算法线**：GLB 里的法线是绑定姿势的，弯过的胳膊腿用旧法线打光会出现条纹。
 * ⚠️ 素模的群众只贴阵列这一层的地：整阵底面在 root 的 y = 0，不逐个对坡面 / 泼溅贴地（DRT-28 只用阵列中心点）。
 */
import {
  Box3,
  BoxGeometry,
  BufferAttribute,
  BufferGeometry,
  Euler,
  Group,
  InstancedMesh,
  Matrix4,
  Mesh,
  MeshBasicMaterial,
  Quaternion,
  Vector3,
  type Material
} from "three";
import { clone as cloneSkinned } from "three/addons/utils/SkeletonUtils.js";
import { DIRECTOR_LIMITS, DIRECTOR_OBJECT_DEFAULTS, directorModelOf, type DirectorModelCode } from "~/utils/directorCatalog";
import { DIRECTOR_MORPH_TARGETS, directorBodyMorphWeights, type DirectorBoneRotation } from "~/utils/directorRig";
import { directorCrowdLayout, directorUniformScale, type DirectorObject } from "~/utils/directorScene";
import { directorLoadMannequin, directorLoadPoses, directorPoseFor, type DirectorPoseData } from "../assets";
import { findSkinned, skinnedBox } from "./Characters";
import { asObject, bool, DEG, num, str, vec3, type EngineContext } from "./context";
import { LAYER_PICK } from "./layers";

/** 一份烘焙好的几何：底面在 y = 0、已按默认身高缩放；`radius` 是离 Y 轴最远的水平距离（米），给包围盒用。 */
type Baked = { geometry: BufferGeometry; height: number; radius: number };

type CacheItem = { promise: Promise<Baked>; baked: Baked | null; refs: number };

export type CrowdEntry = {
  readonly id: string;
  readonly root: Group;
  /** 拾取代理（layer 2）：整阵的包围盒。 */
  readonly proxy: Mesh;
  mesh: InstancedMesh | null;
  /** 正在显示的烘焙几何的键（`素模|姿势`）；还没有时为空串。 */
  bakeKey: string;
  /** 正在烘焙、还没显示的键。 */
  pendingKey: string;
  object: DirectorObject;
  signature: string;
};

/** 素模还没加载出来时代理盒的水平半径（米）：先能选中、能拖。 */
const PLACEHOLDER_RADIUS = 0.3;

const _m = new Matrix4();
const _q = new Quaternion();
const _e = new Euler();
const _v = new Vector3();
const _p = new Vector3();
const _s = new Vector3(1, 1, 1);
const _up = new Vector3(0, 1, 0);

export class Crowds {
  private readonly entries = new Map<string, CrowdEntry>();
  private readonly cache = new Map<string, CacheItem>();
  private readonly pending = new Set<Promise<unknown>>();
  private readonly proxyGeometry: BoxGeometry;
  private readonly proxyMaterial: MeshBasicMaterial;
  private material: Material | null = null;
  private poses: Promise<Map<string, DirectorPoseData>> | null = null;
  private disposed = false;

  constructor(private readonly ctx: EngineContext) {
    this.proxyGeometry = ctx.disposer.geometry(new BoxGeometry(1, 1, 1));
    this.proxyMaterial = ctx.disposer.material(new MeshBasicMaterial({ visible: false }));
    ctx.disposer.defer(() => {
      this.disposed = true;
      // 实例矩阵是挂在 InstancedMesh 上的 GPU 缓冲，不在几何、材质里：网格自己 dispose 才放掉
      for (const entry of this.entries.values()) entry.mesh?.dispose();
    });
  }

  get(id: string | null | undefined): CrowdEntry | undefined {
    return id ? this.entries.get(id) : undefined;
  }

  ids(): string[] {
    return [...this.entries.keys()];
  }

  /** 在途的烘焙全部结束（成功或失败）后 resolve。 */
  async whenIdle(): Promise<void> {
    while (this.pending.size) await Promise.allSettled([...this.pending]);
  }

  /** 拾取目标：可见、未锁定的阵列的代理盒（整阵一个，DR-F-50）。 */
  pickTargets(): Mesh[] {
    const out: Mesh[] = [];
    for (const entry of this.entries.values()) {
      if (entry.object.locked === true || entry.object.visible === false) continue;
      out.push(entry.proxy);
    }
    return out;
  }

  /** 调试 / 检查用：当前显示的实例数与烘焙缓存的键与使用者数。 */
  debugState(): { instances: Record<string, number>; cache: Record<string, number> } {
    const instances: Record<string, number> = {};
    for (const [id, entry] of this.entries) instances[id] = entry.mesh?.count ?? 0;
    const cache: Record<string, number> = {};
    for (const [key, item] of this.cache) cache[key] = item.refs;
    return { instances, cache };
  }

  upsert(object: DirectorObject): CrowdEntry {
    let entry = this.entries.get(object.id);
    if (!entry) entry = this.create(object);
    const signature = JSON.stringify(object);
    if (entry.signature === signature) return entry;
    entry.signature = signature;
    entry.object = object;

    const t = asObject(object.transform);
    const p = vec3(t.position, { x: 0, y: 0, z: 0 });
    const r = vec3(t.rotation, { x: 0, y: 0, z: 0 });
    // 群众同角色：只等比缩放（旧数据三轴不等时统一成等比）。
    const s = directorUniformScale(vec3(t.scale, { x: 1, y: 1, z: 1 })).x;
    entry.root.position.set(p.x, p.y, p.z);
    entry.root.rotation.set(r.x * DEG, r.y * DEG, r.z * DEG, "XYZ");
    entry.root.scale.setScalar(s);
    entry.root.visible = bool(object.visible, true);

    const key = this.bakeKeyOf(object);
    if (key !== entry.bakeKey && key !== entry.pendingKey) this.requestBake(entry, key);
    this.layout(entry);
    this.ctx.invalidate();
    return entry;
  }

  remove(id: string): void {
    const entry = this.entries.get(id);
    if (!entry) return;
    this.entries.delete(id);
    if (entry.pendingKey) this.release(entry.pendingKey);
    if (entry.bakeKey) this.release(entry.bakeKey);
    entry.pendingKey = "";
    entry.bakeKey = "";
    entry.mesh?.dispose();
    entry.root.removeFromParent();
    this.ctx.invalidate();
  }

  private create(object: DirectorObject): CrowdEntry {
    const root = new Group();
    root.name = `crowd:${object.id}`;
    const proxy = new Mesh(this.proxyGeometry, this.proxyMaterial);
    proxy.userData.directorPickId = object.id;
    proxy.layers.set(LAYER_PICK);
    root.add(proxy);
    this.ctx.stage.add(root);
    const entry: CrowdEntry = { id: object.id, root, proxy, mesh: null, bakeKey: "", pendingKey: "", object, signature: "" };
    this.entries.set(object.id, entry);
    return entry;
  }

  private crowdOf(object: DirectorObject) {
    const d = DIRECTOR_OBJECT_DEFAULTS.crowd;
    const c = asObject(object.crowd);
    const model = (directorModelOf(str(c.model, ""))?.code ?? d.model) as DirectorModelCode;
    return {
      model,
      posePreset: str(c.posePreset, d.posePreset),
      rows: num(c.rows, d.rows),
      cols: num(c.cols, d.cols),
      spacing: num(c.spacing, d.spacing),
      jitter: bool(c.jitter, d.jitter),
      seed: num(c.seed, d.seed)
    };
  }

  private bakeKeyOf(object: DirectorObject): string {
    const c = this.crowdOf(object);
    return `${c.model}|${c.posePreset}`;
  }

  /** 换了素模或姿势：烘焙新的那份，好了再换上并释放旧的（换的过程中旧的照常显示，不闪空）。 */
  private requestBake(entry: CrowdEntry, key: string): void {
    if (entry.pendingKey) this.release(entry.pendingKey);
    entry.pendingKey = key;
    const job = this.acquire(key).then(
      (baked) => {
        // 等待期间又换了键、对象被删了、或编辑器关了：这份已经在别处释放过计数，什么都不做
        if (this.disposed || entry.pendingKey !== key || this.entries.get(entry.id) !== entry) return;
        if (entry.bakeKey) this.release(entry.bakeKey);
        entry.bakeKey = key;
        entry.pendingKey = "";
        this.show(entry, baked);
        this.ctx.invalidate();
      },
      (err) => {
        console.error(`[director] 群众烘焙失败（${key}）`, err);
        if (entry.pendingKey === key) entry.pendingKey = "";
      }
    );
    this.track(job);
  }

  private track(p: Promise<unknown>): void {
    this.pending.add(p);
    void p.finally(() => this.pending.delete(p));
  }

  private acquire(key: string): Promise<Baked> {
    let item = this.cache.get(key);
    if (!item) {
      const [model, posePreset] = key.split("|") as [DirectorModelCode, string];
      const created: CacheItem = { promise: this.bake(model, posePreset), baked: null, refs: 0 };
      created.promise.then(
        (baked) => {
          created.baked = baked;
        },
        () => {
          // 失败的不缓存：下次换回这个姿势时能重试
          if (this.cache.get(key) === created) this.cache.delete(key);
        }
      );
      this.cache.set(key, created);
      item = created;
    }
    item.refs++;
    return item.promise;
  }

  /** 少一个使用者；没人用了就释放几何（还在烘焙的等它出来再释放）。 */
  private release(key: string): void {
    const item = this.cache.get(key);
    if (!item) return;
    item.refs--;
    if (item.refs > 0) return;
    this.cache.delete(key);
    if (item.baked) this.ctx.disposer.releaseGeometry(item.baked.geometry);
    else item.promise.then((baked) => this.ctx.disposer.releaseGeometry(baked.geometry), () => undefined);
  }

  private loadPoses(): Promise<Map<string, DirectorPoseData>> {
    if (!this.poses) {
      this.poses = directorLoadPoses().catch((err) => {
        // 姿势库取不到时按静止姿势烘焙（同角色的处理），不让整阵出不来；下次打开编辑器再试。
        console.error("[director] 姿势库加载失败", err);
        return new Map<string, DirectorPoseData>();
      });
    }
    return this.poses;
  }

  /** 烘焙一份 `(素模, 姿势)` 的静态几何（见文件头）。临时克隆用完即弃，不进场景。 */
  private async bake(code: DirectorModelCode, posePreset: string): Promise<Baked> {
    const [gltf, poses] = await Promise.all([directorLoadMannequin(code), this.loadPoses()]);
    const source = findSkinned(gltf.scene);
    if (!source) throw new Error(`素模 ${code} 里没有蒙皮网格`);
    if (!this.material) this.material = this.ctx.disposer.material((source.material as Material).clone());
    const holder = new Group();
    const clone = cloneSkinned(gltf.scene);
    holder.add(clone);
    const skinned = findSkinned(clone);
    if (!skinned) throw new Error(`素模 ${code} 克隆失败`);

    // 静止身高：rest 姿势、不带体型（同角色的口径），用来把身高换算到素模的默认身高
    skinned.morphTargetInfluences?.fill(0);
    const restBox = skinnedBox(skinned, holder, new Box3());
    const restHeight = Math.max(0.1, restBox.max.y - restBox.min.y);

    // 标准体型 + 姿势（覆盖规则同角色：rest × q(Euler XYZ)，R-3 #4）
    const weights = directorBodyMorphWeights(code, "standard");
    const dict = skinned.morphTargetDictionary ?? {};
    const influences = skinned.morphTargetInfluences;
    if (influences) {
      for (const name of DIRECTOR_MORPH_TARGETS) {
        const i = dict[name];
        if (i !== undefined) influences[i] = weights[name];
      }
    }
    const pose = directorPoseFor(poses, posePreset);
    const skeleton = skinned.skeleton;
    const rest = skeleton.bones.map((b) => b.quaternion.clone());
    for (const [name, r] of Object.entries(pose?.bones ?? {}) as Array<[string, DirectorBoneRotation]>) {
      const bone = skeleton.getBoneByName(name);
      if (!bone || ![r?.x, r?.y, r?.z].every((v) => typeof v === "number" && Number.isFinite(v))) continue;
      const index = skeleton.bones.indexOf(bone);
      _e.set(r.x * DEG, r.y * DEG, r.z * DEG, "XYZ");
      bone.quaternion.copy(rest[index] as Quaternion).multiply(_q.setFromEuler(_e));
    }

    // 逐顶点蒙皮（getVertexPosition = morph + applyBoneTransform），换到 holder 坐标
    const posed = skinnedBox(skinned, holder, new Box3());
    _m.copy(holder.matrixWorld).invert().multiply(skinned.matrixWorld);
    const count = skinned.geometry.getAttribute("position").count;
    const scale = (directorModelOf(code)?.heightM ?? 1.7) / restHeight;
    const lift = pose?.lift && pose.lift > 0 ? pose.lift * restHeight : 0;
    const positions = new Float32Array(count * 3);
    let radius = 0;
    for (let i = 0; i < count; i++) {
      skinned.getVertexPosition(i, _v).applyMatrix4(_m);
      const x = _v.x * scale;
      const y = (_v.y - posed.min.y + lift) * scale;
      const z = _v.z * scale;
      positions[i * 3] = x;
      positions[i * 3 + 1] = y;
      positions[i * 3 + 2] = z;
      radius = Math.max(radius, Math.hypot(x, z));
    }
    const geometry = new BufferGeometry();
    geometry.setAttribute("position", new BufferAttribute(positions, 3));
    const index = skinned.geometry.getIndex();
    if (index) geometry.setIndex(index.clone());
    geometry.computeVertexNormals();
    geometry.computeBoundingBox();
    geometry.computeBoundingSphere();
    const height = (posed.max.y - posed.min.y + lift) * scale;
    // 临时克隆：骨骼纹理只在渲染时才建，没渲染过就没有；照样 dispose 一次，不留尾巴
    skeleton.dispose();
    holder.clear();
    return { geometry: this.ctx.disposer.geometry(geometry), height, radius };
  }

  /** 换上新烘焙的几何：首次建实例网格；以后只换几何（实例矩阵不变）。 */
  private show(entry: CrowdEntry, baked: Baked): void {
    if (!entry.mesh) {
      const material = this.material as Material;
      const mesh = new InstancedMesh(baked.geometry, material, DIRECTOR_LIMITS.maxCrowdSize);
      mesh.name = `crowd-instances:${entry.id}`;
      mesh.castShadow = true;
      mesh.receiveShadow = true;
      mesh.count = 0;
      entry.mesh = mesh;
      entry.root.add(mesh);
    } else {
      entry.mesh.geometry = baked.geometry;
    }
    entry.mesh.userData.baked = baked;
    this.layout(entry);
  }

  /** 按当前参数摆放实例、更新包围盒代理。烘焙还没好时只摆代理（先能选中、能拖）。 */
  private layout(entry: CrowdEntry): void {
    const c = this.crowdOf(entry.object);
    const slots = directorCrowdLayout(c);
    const baked = (entry.mesh?.userData.baked as Baked | undefined) ?? null;
    const mesh = entry.mesh;
    if (mesh) {
      slots.forEach((slot, i) => {
        _q.setFromAxisAngle(_up, slot.yaw * DEG);
        mesh.setMatrixAt(i, _m.compose(_p.set(slot.x, 0, slot.z), _q, _s));
      });
      mesh.count = slots.length;
      mesh.instanceMatrix.needsUpdate = true;
      // 视锥剔除按实例的包围球算：位置变了要重算，否则镜头边上的人会被剔掉
      mesh.computeBoundingBox();
      mesh.computeBoundingSphere();
    }
    const radius = baked?.radius ?? PLACEHOLDER_RADIUS;
    const height = baked?.height ?? directorModelOf(c.model)?.heightM ?? 1.7;
    let minX = Infinity;
    let maxX = -Infinity;
    let minZ = Infinity;
    let maxZ = -Infinity;
    for (const slot of slots) {
      minX = Math.min(minX, slot.x);
      maxX = Math.max(maxX, slot.x);
      minZ = Math.min(minZ, slot.z);
      maxZ = Math.max(maxZ, slot.z);
    }
    entry.proxy.position.set((minX + maxX) / 2, height / 2, (minZ + maxZ) / 2);
    entry.proxy.scale.set(maxX - minX + radius * 2, Math.max(0.05, height), maxZ - minZ + radius * 2);
  }
}
