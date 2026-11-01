/**
 * 场景里的机位（DR-F-22 / 23 / 24，dev/03 §4.2 ShotCameras）。
 *
 * 结构：`root`（transform：位置 + 偏航俯仰，世界坐标，不受场景整体变换影响）→ `rollNode`（`camera.roll` 绕视线轴）
 * → PerspectiveCamera + 机位模型 + 视锥线。机位模型与视锥只在 layer 1（编辑辅助）。
 *
 * - 竖直视角由焦距与传感器长边推导（`directorVerticalFov`），宽高比按机位画幅，不按视口：机位视角与截图都按画幅出图，
 *   视口只是用 letterbox 把它摆进来（DR-AC-05）。
 * - 横滚单独一层：`transform.rotation` 只含偏航与俯仰（Go 求解器同口径，dev/01 R-3 #7），拖动旋转手柄改的是 root，
 *   不会把横滚混进欧拉角。
 * - 机位不缩放（DR-F-33）：`transform.scale` 不作用到 three 对象上。
 * - `visible = false` 只隐藏机位模型，机位照样能用来看、能截图（指纹也照样计入，dev/02 R-1 #4）。
 */
import {
  BoxGeometry,
  BufferGeometry,
  EdgesGeometry,
  Float32BufferAttribute,
  Group,
  LineBasicMaterial,
  LineSegments,
  Mesh,
  MeshBasicMaterial,
  PerspectiveCamera
} from "three";
import { DIRECTOR_OBJECT_DEFAULTS } from "~/utils/directorCatalog";
import { directorAspectRatio, directorVerticalFov } from "~/utils/directorCamera";
import type { DirectorObject } from "~/utils/directorScene";
import { asObject, bool, DEG, num, str, vec3, type EngineContext } from "./context";
import { directorSetLayer, HELPER_MAGENTA, LAYER_CONTENT, LAYER_HELPERS } from "./layers";

/** 视锥线画到多远（米）：只示意朝向与视角，不画到远裁剪面。 */
const FRUSTUM_LENGTH = 1.2;

export type ShotCamera = {
  readonly id: string;
  readonly root: Group;
  readonly rollNode: Group;
  readonly camera: PerspectiveCamera;
  /** 机位模型（layer 1），也是拾取目标（DRT-13）。 */
  readonly model: Group;
  readonly body: Mesh;
  /** 机位画幅的宽高比。 */
  aspect: number;
  aspectCode: string;
  object: DirectorObject;
  signature: string;
  readonly frustum: LineSegments;
};

export class ShotCameras {
  private readonly entries = new Map<string, ShotCamera>();
  private readonly bodyGeometry: BoxGeometry;
  private readonly edgeGeometry: EdgesGeometry;
  private readonly upGeometry: BufferGeometry;
  private readonly bodyMaterial: MeshBasicMaterial;
  private readonly lineMaterial: LineBasicMaterial;
  private readonly frustumMaterial: LineBasicMaterial;

  constructor(private readonly ctx: EngineContext) {
    const d = ctx.disposer;
    // 机身在镜头后方（机位朝 −Z 看，机身向 +Z 延伸），原点就是镜头光心。
    this.bodyGeometry = d.geometry(new BoxGeometry(0.16, 0.12, 0.22).translate(0, 0, 0.11));
    this.edgeGeometry = d.geometry(new EdgesGeometry(this.bodyGeometry));
    // 机身顶上的小三角，指示机位的「上」（横滚时跟着转）。
    this.upGeometry = d.geometry(
      new BufferGeometry().setAttribute(
        "position",
        new Float32BufferAttribute([-0.05, 0.08, 0.05, 0.05, 0.08, 0.05, 0.05, 0.08, 0.05, 0, 0.14, 0.05, 0, 0.14, 0.05, -0.05, 0.08, 0.05], 3)
      )
    );
    this.bodyMaterial = d.material(new MeshBasicMaterial({ color: HELPER_MAGENTA, transparent: true, opacity: 0.35, depthWrite: false }));
    this.lineMaterial = d.material(new LineBasicMaterial({ color: HELPER_MAGENTA }));
    this.frustumMaterial = d.material(new LineBasicMaterial({ color: HELPER_MAGENTA, transparent: true, opacity: 0.6 }));
  }

  get(id: string | null | undefined): ShotCamera | undefined {
    return id ? this.entries.get(id) : undefined;
  }

  list(): ShotCamera[] {
    return [...this.entries.values()];
  }

  ids(): string[] {
    return [...this.entries.keys()];
  }

  /** 按场景对象新建或更新机位。`skipTransform` 为真时保留 three 对象上的位置与朝向（正在走位的那个机位）。 */
  upsert(object: DirectorObject, skipTransform = false): ShotCamera {
    let entry = this.entries.get(object.id);
    if (!entry) entry = this.create(object);
    const signature = JSON.stringify(object);
    if (entry.signature === signature) return entry;
    entry.signature = signature;
    entry.object = object;

    const t = asObject(object.transform);
    if (!skipTransform) {
      const p = vec3(t.position, { x: 0, y: 0, z: 0 });
      const r = vec3(t.rotation, { x: 0, y: 0, z: 0 });
      entry.root.position.set(p.x, p.y, p.z);
      entry.root.rotation.set(r.x * DEG, r.y * DEG, r.z * DEG, "XYZ");
    }

    const d = DIRECTOR_OBJECT_DEFAULTS.camera;
    const c = asObject(object.camera);
    const aspectCode = str(c.aspect, d.aspect);
    let aspect: number;
    try {
      aspect = directorAspectRatio(aspectCode);
    } catch {
      aspect = directorAspectRatio(d.aspect);
    }
    const focal = num(c.focalLengthMm, d.focalLengthMm);
    const sensor = num(c.sensorWidthMm, d.sensorWidthMm);
    const near = num(c.near, d.near);
    const far = num(c.far, d.far);
    const cam = entry.camera;
    cam.fov = directorVerticalFov(focal > 0 ? focal : d.focalLengthMm, sensor > 0 ? sensor : d.sensorWidthMm, aspect);
    cam.aspect = aspect;
    cam.near = near > 0 ? near : d.near;
    cam.far = far > cam.near ? far : Math.max(cam.near * 10, d.far);
    cam.updateProjectionMatrix();
    entry.rollNode.rotation.set(0, 0, num(c.roll, d.roll) * DEG);
    entry.aspect = aspect;
    entry.aspectCode = aspectCode;
    this.updateFrustum(entry);
    entry.model.visible = bool(object.visible, true);
    this.ctx.invalidate();
    return entry;
  }

  remove(id: string): void {
    const entry = this.entries.get(id);
    if (!entry) return;
    this.entries.delete(id);
    entry.root.removeFromParent();
    this.ctx.disposer.releaseGeometry(entry.frustum.geometry);
    this.ctx.invalidate();
  }

  /** 拾取目标：可见、未锁定的机位的机身（DRT-13）。 */
  pickTargets(except?: string | null): Mesh[] {
    const out: Mesh[] = [];
    for (const entry of this.entries.values()) {
      if (entry.id === except || entry.object.locked === true || entry.object.visible === false) continue;
      out.push(entry.body);
    }
    return out;
  }

  private create(object: DirectorObject): ShotCamera {
    const root = new Group();
    root.name = `camera:${object.id}`;
    const rollNode = new Group();
    const camera = new PerspectiveCamera(40, 16 / 9, 0.1, 1000);
    camera.layers.set(LAYER_CONTENT);
    camera.layers.enable(LAYER_HELPERS);
    const model = new Group();
    const body = new Mesh(this.bodyGeometry, this.bodyMaterial);
    body.userData.directorPickId = object.id;
    const edges = new LineSegments(this.edgeGeometry, this.lineMaterial);
    const up = new LineSegments(this.upGeometry, this.lineMaterial);
    // 4 条棱 + 4 条框 = 16 个端点；顶点缓冲只建一次，改焦距 / 画幅时原地改写（换新属性会让旧的 GL 缓冲留到几何释放）。
    const frustumGeometry = new BufferGeometry().setAttribute("position", new Float32BufferAttribute(new Float32Array(16 * 3), 3));
    const frustum = new LineSegments(this.ctx.disposer.geometry(frustumGeometry), this.frustumMaterial);
    model.add(body, edges, up, frustum);
    directorSetLayer(model, LAYER_HELPERS);
    rollNode.add(camera, model);
    root.add(rollNode);
    this.ctx.scene.add(root);
    const entry: ShotCamera = {
      id: object.id,
      root,
      rollNode,
      camera,
      model,
      body,
      aspect: 16 / 9,
      aspectCode: "16:9",
      object,
      signature: "",
      frustum
    };
    this.entries.set(object.id, entry);
    return entry;
  }

  /** 视锥线：光心到 `FRUSTUM_LENGTH` 处画面四角的四条棱，加上那一处的画框。 */
  private updateFrustum(entry: ShotCamera): void {
    const halfH = Math.tan((entry.camera.fov * DEG) / 2) * FRUSTUM_LENGTH;
    const halfW = halfH * entry.aspect;
    const z = -FRUSTUM_LENGTH;
    const corners = [
      [-halfW, -halfH, z],
      [halfW, -halfH, z],
      [halfW, halfH, z],
      [-halfW, halfH, z]
    ] as const;
    const points: number[] = [];
    for (const c of corners) points.push(0, 0, 0, c[0], c[1], c[2]);
    for (let i = 0; i < 4; i++) {
      const a = corners[i] as readonly [number, number, number];
      const b = corners[(i + 1) % 4] as readonly [number, number, number];
      points.push(a[0], a[1], a[2], b[0], b[1], b[2]);
    }
    const geometry = entry.frustum.geometry;
    const position = geometry.getAttribute("position") as Float32BufferAttribute;
    (position.array as Float32Array).set(points);
    position.needsUpdate = true;
    geometry.computeBoundingSphere();
  }
}
