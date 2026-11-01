/**
 * 几何道具（DR-F-51，dev/03 §4.2 Props、§9.4）：方块、圆柱、球、平面。
 *
 * - 结构：`root`（transform，stage 局部坐标，底面中心为锚点）→ 网格（`scale = prop.size`）。缩放可三轴分开（DR-F-33）；
 * - 单位几何体全场共享一份，底面在 y = 0：方块、圆柱、球的包围盒都是 1×1×1；平面是水平放在地上的 1×1（尺寸取 x 与 z，
 *   竖起来当墙或背板就转 90°）；
 * - 材质按颜色共享（白模灰阶，金属度 0、粗糙度 0.8，与素模同风格）；
 * - 共享几何各算一次 BVH（three-mesh-bvh），拾取与贴地射线都走它（DRT-13）。
 */
import {
  BoxGeometry,
  CylinderGeometry,
  DoubleSide,
  FrontSide,
  Group,
  Mesh,
  MeshStandardMaterial,
  PlaneGeometry,
  SphereGeometry,
  type BufferGeometry
} from "three";
import { acceleratedRaycast, MeshBVH } from "three-mesh-bvh";
import { DIRECTOR_OBJECT_DEFAULTS, type DirectorPropShape } from "~/utils/directorCatalog";
import type { DirectorObject } from "~/utils/directorScene";
import { asObject, bool, DEG, str, vec3, type EngineContext } from "./context";

export type PropEntry = {
  readonly id: string;
  readonly root: Group;
  mesh: Mesh;
  shape: DirectorPropShape;
  object: DirectorObject;
  signature: string;
};

const SHAPES: readonly DirectorPropShape[] = ["box", "cylinder", "sphere", "plane"];

function unitGeometry(shape: DirectorPropShape): BufferGeometry {
  switch (shape) {
    case "cylinder":
      return new CylinderGeometry(0.5, 0.5, 1, 32).translate(0, 0.5, 0);
    case "sphere":
      return new SphereGeometry(0.5, 32, 16).translate(0, 0.5, 0);
    case "plane":
      return new PlaneGeometry(1, 1).rotateX(-Math.PI / 2);
    default:
      return new BoxGeometry(1, 1, 1).translate(0, 0.5, 0);
  }
}

export class Props {
  private readonly entries = new Map<string, PropEntry>();
  private readonly geometries = new Map<DirectorPropShape, BufferGeometry>();
  private readonly materials = new Map<string, MeshStandardMaterial>();

  constructor(private readonly ctx: EngineContext) {}

  get(id: string | null | undefined): PropEntry | undefined {
    return id ? this.entries.get(id) : undefined;
  }

  ids(): string[] {
    return [...this.entries.keys()];
  }

  /** 全部道具网格（贴地射线的目标，DRT-13）。 */
  meshes(): Mesh[] {
    return [...this.entries.values()].filter((e) => e.object.visible !== false).map((e) => e.mesh);
  }

  /** 拾取目标：可见、未锁定的道具。 */
  pickTargets(): Mesh[] {
    return [...this.entries.values()].filter((e) => e.object.locked !== true && e.object.visible !== false).map((e) => e.mesh);
  }

  upsert(object: DirectorObject): PropEntry {
    const d = DIRECTOR_OBJECT_DEFAULTS.prop;
    const p = asObject(object.prop);
    const rawShape = str(p.shape, d.shape) as DirectorPropShape;
    const shape = SHAPES.includes(rawShape) ? rawShape : d.shape;
    let entry = this.entries.get(object.id);
    if (!entry) {
      const root = new Group();
      root.name = `prop:${object.id}`;
      const mesh = this.createMesh(object.id, shape);
      root.add(mesh);
      this.ctx.stage.add(root);
      entry = { id: object.id, root, mesh, shape, object, signature: "" };
      this.entries.set(object.id, entry);
    }
    const signature = JSON.stringify(object);
    if (entry.signature === signature) return entry;
    entry.signature = signature;
    entry.object = object;

    if (entry.shape !== shape) {
      entry.mesh.removeFromParent();
      entry.mesh = this.createMesh(object.id, shape);
      entry.root.add(entry.mesh);
      entry.shape = shape;
    }
    const t = asObject(object.transform);
    const pos = vec3(t.position, { x: 0, y: 0, z: 0 });
    const rot = vec3(t.rotation, { x: 0, y: 0, z: 0 });
    const scale = vec3(t.scale, { x: 1, y: 1, z: 1 });
    entry.root.position.set(pos.x, pos.y, pos.z);
    entry.root.rotation.set(rot.x * DEG, rot.y * DEG, rot.z * DEG, "XYZ");
    entry.root.scale.set(scale.x, scale.y, scale.z);
    const size = vec3(p.size, d.size);
    entry.mesh.scale.set(Math.max(0.01, size.x), Math.max(0.01, size.y), Math.max(0.01, size.z));
    entry.mesh.material = this.material(str(p.color, d.color), shape === "plane");
    entry.root.visible = bool(object.visible, true);
    this.ctx.invalidate();
    return entry;
  }

  remove(id: string): void {
    const entry = this.entries.get(id);
    if (!entry) return;
    this.entries.delete(id);
    entry.root.removeFromParent();
    this.ctx.invalidate();
  }

  private createMesh(id: string, shape: DirectorPropShape): Mesh {
    const mesh = new Mesh(this.geometry(shape));
    mesh.castShadow = true;
    mesh.receiveShadow = true;
    mesh.userData.directorPickId = id;
    mesh.raycast = acceleratedRaycast;
    return mesh;
  }

  private geometry(shape: DirectorPropShape): BufferGeometry {
    let g = this.geometries.get(shape);
    if (!g) {
      g = this.ctx.disposer.geometry(unitGeometry(shape));
      // 共享几何的 BVH 只算一次；释放时随几何一起丢掉（Disposer）。
      g.boundsTree = new MeshBVH(g);
      this.geometries.set(shape, g);
    }
    return g;
  }

  private material(color: string, doubleSided: boolean): MeshStandardMaterial {
    const key = `${color}|${doubleSided ? 2 : 1}`;
    let m = this.materials.get(key);
    if (!m) {
      m = this.ctx.disposer.material(
        new MeshStandardMaterial({ color, roughness: 0.8, metalness: 0, side: doubleSided ? DoubleSide : FrontSide })
      );
      this.materials.set(key, m);
    }
    return m;
  }
}
