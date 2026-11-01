/**
 * 视口点选（DR-F-10 / 11 / 30，dev/03 §4.2 Picking）。
 *
 * - 拾取目标由调用方按当前状态给出（只含可见、未锁定的对象，DR-F-10）：角色的代理盒（layer 2）、道具网格（BVH）、
 *   群众整阵一个包围盒代理（layer 2，DRT-22：阵列作为一个对象整体选中、移动，不按 `instanceId` 反查个体）、
 *   机位机身（layer 1 的可见模型）。射线对全部图层开放，范围由目标清单决定。
 * - 点击才算选：按下到抬起移动不超过 5 像素。拖动视角、拖动变换手柄都不改选中（手柄按下时由 TransformTool 通知 `suppress`）。
 * - 点空白处取消选中；机位视角下点到画幅框外的黑边，什么都不做。
 * - `groundPointAtViewCenter`：视口（机位视角下是画幅框）中心的视线与地面的交点，给「添加对象」用（DR-F-11）。
 */
import { Matrix4, Plane, Ray, Raycaster, Vector2, Vector3, type Camera, type Group, type Object3D } from "three";
import type { DirectorRect } from "~/utils/directorCamera";
import type { DirectorVec3 } from "~/utils/directorScene";
import { round6 } from "./context";
import type { Disposer } from "./Disposer";

const CLICK_SLOP_PX = 5;
/** 视线与地面的交点超出这个距离（米）就当没打中：贴着地平线看时交点会在几公里外。 */
const MAX_GROUND_DISTANCE = 80;

export interface PickingDeps {
  /** 当前用来看的相机（导演相机或机位）。 */
  camera(): Camera;
  /** 当前画面在视口里的矩形（CSS 像素，左上角为原点）。 */
  frame(): DirectorRect;
  targets(): Object3D[];
  onPick(id: string | null): void;
}

const _ndc = new Vector2();
const _plane = new Plane(new Vector3(0, 1, 0), 0);
const _ray = new Ray();
const _inv = new Matrix4();
const _hit = new Vector3();

/** 从命中的物体往上找所属的场景对象 ID。 */
export function directorPickIdOf(object: Object3D | null): string | null {
  for (let o = object; o; o = o.parent) {
    const id = o.userData.directorPickId;
    if (typeof id === "string") return id;
  }
  return null;
}

export class Picking {
  enabled = true;
  private readonly raycaster = new Raycaster();
  private down: { id: number; x: number; y: number } | null = null;
  private suppressed = false;

  constructor(
    private readonly canvas: HTMLCanvasElement,
    private readonly deps: PickingDeps,
    disposer: Disposer
  ) {
    this.raycaster.layers.enableAll();
    this.raycaster.firstHitOnly = true;
    canvas.addEventListener("pointerdown", this.onPointerDown);
    canvas.addEventListener("pointerup", this.onPointerUp);
    canvas.addEventListener("pointercancel", this.onPointerCancel);
    disposer.defer(() => {
      canvas.removeEventListener("pointerdown", this.onPointerDown);
      canvas.removeEventListener("pointerup", this.onPointerUp);
      canvas.removeEventListener("pointercancel", this.onPointerCancel);
    });
  }

  /** 这一次按下不算点选（按在了变换手柄上）。 */
  suppress(): void {
    this.suppressed = true;
  }

  /** 视口坐标（CSS 像素，相对 canvas 左上角）下的拾取；画幅框外返回 undefined。 */
  pickAt(x: number, y: number): string | null | undefined {
    const f = this.deps.frame();
    if (x < f.x || y < f.y || x > f.x + f.width || y > f.y + f.height || f.width <= 0 || f.height <= 0) return undefined;
    _ndc.set(((x - f.x) / f.width) * 2 - 1, -((y - f.y) / f.height) * 2 + 1);
    this.raycaster.setFromCamera(_ndc, this.deps.camera());
    const hit = this.raycaster.intersectObjects(this.deps.targets(), false)[0];
    return hit ? directorPickIdOf(hit.object) : null;
  }

  /** 视口中心的视线与地面（stage 局部 y = groundY）的交点，stage 局部坐标；打不中或太远回 null。 */
  groundPointAtCenter(stage: Group, groundY: number): DirectorVec3 | null {
    this.raycaster.setFromCamera(_ndc.set(0, 0), this.deps.camera());
    stage.updateWorldMatrix(true, false);
    _inv.copy(stage.matrixWorld).invert();
    _ray.copy(this.raycaster.ray).applyMatrix4(_inv);
    _plane.set(new Vector3(0, 1, 0), -groundY);
    const point = _ray.intersectPlane(_plane, _hit);
    if (!point) return null;
    // 距离按世界坐标算（stage 有缩放时局部距离不是用户看到的距离）。
    const world = point.clone().applyMatrix4(stage.matrixWorld);
    if (world.distanceTo(this.raycaster.ray.origin) > MAX_GROUND_DISTANCE) return null;
    return { x: round6(point.x), y: round6(groundY), z: round6(point.z) };
  }

  /**
   * ⚠️ 这里不清 `suppressed`：变换手柄的监听挂得更早（TransformTool 先于 Picking 创建），按在手柄上时
   *    它在这一次按下里已经调过 `suppress`。标记在抬起时清。
   */
  private readonly onPointerDown = (event: PointerEvent) => {
    if (event.button !== 0) return;
    this.down = { id: event.pointerId, x: event.clientX, y: event.clientY };
  };

  private readonly onPointerCancel = () => {
    this.down = null;
    this.suppressed = false;
  };

  private readonly onPointerUp = (event: PointerEvent) => {
    const down = this.down;
    this.down = null;
    const suppressed = this.suppressed;
    this.suppressed = false;
    if (!down || down.id !== event.pointerId || suppressed || !this.enabled) return;
    if (Math.hypot(event.clientX - down.x, event.clientY - down.y) > CLICK_SLOP_PX) return;
    const rect = this.canvas.getBoundingClientRect();
    const id = this.pickAt(event.clientX - rect.left, event.clientY - rect.top);
    if (id !== undefined) this.deps.onPick(id);
  };
}
