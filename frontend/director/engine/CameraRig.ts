/**
 * 导演视角与自由走位（DR-F-20 / 21 / 25，dev/03 §4.2 CameraRig）。
 *
 * - 导演视角：PerspectiveCamera + camera-controls（旋转 / 平移 / 缩放 / 过渡），右上角轴向小方块（three-viewport-gizmo）；
 * - 重置视角：按全部可见对象的包围球，从固定的「右前上方」看过去，距离由 `getDistanceToFitSphere` 算，
 *   保证包围球整个落在视锥里（DR-AC-05）。camera-controls 自带的 `fitToBox` 会把视角吸到最近的坐标轴上，
 *   重置成正视图反而看不出前后关系，所以不用它；
 * - 自由走位（WalkController）：机位视角下移动的是**机位本身**（DR-F-25），松开按键 300ms 后发一次 `camera-moved`。
 *
 * ⚠️ camera-controls 必须先 `install({THREE})`，而且只能 install 一次：在模块顶层装，不要每次挂载都装。
 * ⚠️ 两个控件都靠渲染循环驱动：camera-controls 的阻尼在 `update(dt)` 里推进，轴向小方块的过渡动画在它的
 *    `render()` 里推进。按需渲染下（DRD-35）要把「还在动」报给循环，否则动画只走一帧就停住。
 */
import CameraControls from "camera-controls";
import {
  Box3,
  Euler,
  Matrix4,
  PerspectiveCamera,
  Quaternion,
  Raycaster,
  Sphere,
  Spherical,
  Vector2,
  Vector3,
  Vector4,
  type Object3D
} from "three";
import { ViewportGizmo } from "three-viewport-gizmo";
import { DIRECTOR_WALK_SPEED, directorLightPosition, directorWalkStep } from "~/utils/directorCamera";
import { DEG } from "./context";
import type { Disposer } from "./Disposer";
import { LAYER_CONTENT, LAYER_HELPERS } from "./layers";
import type { RendererHost } from "./RendererHost";

CameraControls.install({ THREE: { Vector2, Vector3, Vector4, Quaternion, Matrix4, Spherical, Box3, Sphere, Raycaster } });

/** 重置视角的方向：方位 30°（从角色正面偏右）、俯仰 20°。 */
const RESET_AZIMUTH = 30;
const RESET_ELEVATION = 20;
/** 包围球的最小半径：空场景或只有一个小道具时，别把相机怼到 10 厘米外。 */
const MIN_FIT_RADIUS = 1.2;

export function directorPrefersReducedMotion(): boolean {
  return typeof window !== "undefined" && typeof window.matchMedia === "function"
    ? window.matchMedia("(prefers-reduced-motion: reduce)").matches
    : false;
}

export class CameraRig {
  readonly camera = new PerspectiveCamera(40, 1, 0.05, 2000);
  readonly controls: CameraControls;
  readonly gizmo: ViewportGizmo;
  /** 用户动过导演视角之后，首份场景加载完成时不再自动重置（不抢用户已经调好的视角）。 */
  touched = false;

  private active = true;
  private readonly gizmoElement: HTMLElement | null;

  constructor(
    private readonly host: RendererHost,
    container: HTMLElement,
    disposer: Disposer
  ) {
    this.camera.layers.set(LAYER_CONTENT);
    this.camera.layers.enable(LAYER_HELPERS);

    // 先不连 DOM：变换手柄（TransformControls）要先连，按下手柄时它先关掉 camera-controls（DRT-13）。
    const controls = new CameraControls(this.camera);
    controls.dollyToCursor = true;
    controls.minDistance = 0.2;
    controls.maxDistance = 600;
    controls.setLookAt(4, 3, 6, 0, 0.9, 0, false);
    this.controls = controls;

    const reduced = directorPrefersReducedMotion();
    this.gizmo = new ViewportGizmo(this.camera, host.renderer, {
      container,
      type: "cube",
      size: 96,
      placement: "top-right",
      offset: { top: 12, right: 12 },
      animated: !reduced,
      className: "director-viewport-gizmo"
    });
    this.gizmoElement = container.querySelector<HTMLElement>(".director-viewport-gizmo");

    // camera-controls 与轴向小方块互相同步（three-viewport-gizmo README 里 camera-controls 的接法）。
    controls.addEventListener("update", this.onControlsUpdate);
    controls.addEventListener("control", this.onControl);
    controls.addEventListener("controlstart", this.onControl);
    controls.addEventListener("transitionstart", this.onTransitionStart);
    this.gizmo.addEventListener("start", this.onGizmoStart);
    this.gizmo.addEventListener("end", this.onGizmoEnd);
    this.gizmo.addEventListener("change", this.onGizmoChange);
    window.addEventListener("pointerup", this.onWindowPointerUp);

    disposer.defer(() => {
      window.removeEventListener("pointerup", this.onWindowPointerUp);
      controls.removeEventListener("update", this.onControlsUpdate);
      controls.removeEventListener("control", this.onControl);
      controls.removeEventListener("controlstart", this.onControl);
      controls.removeEventListener("transitionstart", this.onTransitionStart);
      this.gizmo.removeEventListener("start", this.onGizmoStart);
      this.gizmo.removeEventListener("end", this.onGizmoEnd);
      this.gizmo.removeEventListener("change", this.onGizmoChange);
      controls.dispose();
      // ⚠️ 2.2.0 的 `gizmo.dispose()` 只处理直接子物体，而且边遍历边移除（forEach 里 remove）会隔一个跳一个：
      //    实测关闭后剩 2 张纹理。先把它整棵子树交给 Disposer 兜底，再调它自己的 dispose 移除 DOM。
      disposer.collect(this.gizmo);
      this.gizmo.dispose();
    });
  }

  private readonly onControlsUpdate = () => {
    this.controls.getTarget(this.gizmo.target);
    this.gizmo.update();
  };

  /** 用户在拖动或滚轮（`control` 只由用户输入触发）：下一帧 `update(dt)` 会把输入推进到相机上。 */
  private readonly onControl = () => {
    this.touched = true;
    this.host.invalidate();
  };

  private readonly onTransitionStart = () => this.host.invalidate();

  private readonly onGizmoStart = () => {
    this.touched = true;
    this.controls.enabled = false;
  };

  private readonly onGizmoEnd = () => {
    this.controls.enabled = this.active;
  };

  private readonly onGizmoChange = () => {
    const p = this.camera.position;
    void this.controls.setPosition(p.x, p.y, p.z, false);
    this.host.invalidate();
  };

  /**
   * ⚠️ 按下轴向小方块又没点中任何轴时，它发了 start 却永远不发 end（2.2.0 的行为），camera-controls 就一直被禁用。
   * 在 window 上兜底：它的 pointerup 监听挂在 document 上、先于这里执行；这时没有在过渡就说明没点中。
   */
  private readonly onWindowPointerUp = () => {
    if (!this.gizmo.animating) this.controls.enabled = this.active;
  };

  connect(canvas: HTMLElement): void {
    this.controls.connect(canvas);
  }

  /** 推进阻尼与过渡；返回 true 表示还在动，渲染循环要继续。 */
  update(dt: number): boolean {
    if (!this.active) return false;
    // 小方块过渡期间相机由它直接摆，camera-controls 的旧状态不要回写（会来回拉扯一帧）。
    if (this.gizmo.animating) return true;
    return this.controls.update(dt);
  }

  /** 画轴向小方块（在主视口之后画；它自己管视口与裁剪，只清深度）。 */
  renderGizmo(): void {
    if (this.active) this.gizmo.render();
  }

  resize(width: number, height: number): void {
    this.camera.aspect = width / Math.max(1, height);
    this.camera.updateProjectionMatrix();
    this.gizmo.update();
  }

  /** 机位视角下停用导演相机的控件与小方块（DR-F-20）。 */
  setActive(on: boolean): void {
    this.active = on;
    this.controls.enabled = on;
    this.gizmo.enabled = on;
    if (this.gizmoElement) this.gizmoElement.style.display = on ? "" : "none";
    if (on) this.gizmo.update();
  }

  isActive(): boolean {
    return this.active;
  }

  /** 重置视角：从固定方向看全包围球（DR-F-21）。 */
  resetView(bounds: Sphere, transition = !directorPrefersReducedMotion()): void {
    const radius = Math.max(bounds.radius, MIN_FIT_RADIUS);
    const distance = this.controls.getDistanceToFitSphere(radius) * 1.05;
    const dir = directorLightPosition(RESET_AZIMUTH, RESET_ELEVATION, 1);
    const c = bounds.center;
    void this.controls.setLookAt(c.x + dir.x * distance, c.y + dir.y * distance, c.z + dir.z * distance, c.x, c.y, c.z, transition);
    this.host.invalidate();
  }

  /** 聚焦一个对象：保持当前朝向，只移动注视点与距离。 */
  focus(bounds: Sphere, transition = !directorPrefersReducedMotion()): void {
    const sphere = bounds.clone();
    sphere.radius = Math.max(sphere.radius, 0.4);
    void this.controls.fitToSphere(sphere, transition);
    this.host.invalidate();
  }
}

/**
 * 自由走位（DR-F-25，DR-AC-06）：WASD 前后左右、E / Q 升降、Shift 三倍速，鼠标拖动转向。
 *
 * - 按键状态由编辑器经 `key()` 交进来（它已在捕获阶段独占键盘）；`clear()` 在窗口失焦、页面隐藏时调用 ——
 *   引擎自己也监听这两件事兜底：Storyboarder 出过「点到别的窗口后相机一直往一个方向飞」的事故。
 * - 走位途中只改 three 对象，不写 store；松开全部方向键（或拖动转向结束）300ms 后调一次 `onSettled`，
 *   由引擎发 `camera-moved`（DRD-36：一次走位只记一步撤销）。
 * - 转向用偏航 + 俯仰（俯仰夹在 ±89°），写回时换成场景用的 XYZ 欧拉角；横滚在 `camera.roll` 里，不受影响。
 */
export const DIRECTOR_WALK_KEYS: ReadonlySet<string> = new Set(["KeyW", "KeyA", "KeyS", "KeyD", "KeyE", "KeyQ", "ShiftLeft", "ShiftRight"]);
const MOVE_KEYS = ["KeyW", "KeyA", "KeyS", "KeyD", "KeyE", "KeyQ"];
const SETTLE_MS = 300;
/** 拖动转向的灵敏度：每像素 0.2°。 */
const TURN_DEG_PER_PX = 0.2;

const _forward = new Vector3();
const _euler = new Euler(0, 0, 0, "YXZ");

export class WalkController {
  private readonly keys = new Set<string>();
  private target: Object3D | null = null;
  private enabled = false;
  private moved = false;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private drag: { id: number; x: number; y: number; yaw: number; pitch: number } | null = null;

  constructor(
    private readonly host: RendererHost,
    private readonly canvas: HTMLCanvasElement,
    disposer: Disposer,
    private readonly onSettled: () => void
  ) {
    canvas.addEventListener("pointerdown", this.onPointerDown);
    window.addEventListener("blur", this.onBlur);
    document.addEventListener("visibilitychange", this.onVisibility);
    disposer.defer(() => {
      this.cancelTimer();
      canvas.removeEventListener("pointerdown", this.onPointerDown);
      canvas.removeEventListener("pointermove", this.onPointerMove);
      canvas.removeEventListener("pointerup", this.onPointerUp);
      canvas.removeEventListener("pointercancel", this.onPointerUp);
      window.removeEventListener("blur", this.onBlur);
      document.removeEventListener("visibilitychange", this.onVisibility);
    });
  }

  /** 开始 / 结束走位。结束时还没发出去的 `camera-moved` 立刻发掉，不丢这一步。 */
  setEnabled(on: boolean, target: Object3D | null): void {
    if (!on) {
      this.clear();
      this.endDrag();
      this.flush();
      this.enabled = false;
      this.target = null;
      this.host.setReason("walk", false);
      return;
    }
    if (this.target && this.target !== target) this.flush();
    this.enabled = true;
    this.target = target;
  }

  isEnabled(): boolean {
    return this.enabled;
  }

  /** 正在走（有方向键按着或正在拖动转向）或者还没发 `camera-moved` 的机位。 */
  busyTarget(): Object3D | null {
    return this.enabled && (this.moving() || this.drag || this.moved) ? this.target : null;
  }

  key(code: string, down: boolean): void {
    if (!DIRECTOR_WALK_KEYS.has(code)) return;
    if (down) {
      if (!this.enabled || !this.target) return;
      this.keys.add(code);
      if (this.moving()) {
        this.cancelTimer();
        this.host.setReason("walk", true);
      }
      return;
    }
    this.keys.delete(code);
    if (!this.moving()) this.release();
  }

  /** 清空按键状态（窗口失焦、页面隐藏、Esc 退出时）。 */
  clear(): void {
    if (this.keys.size === 0) return;
    this.keys.clear();
    this.release();
  }

  /** 每帧推进：按 `dt` 走一步。返回 true 表示还在走。 */
  update(dt: number): boolean {
    if (!this.enabled || !this.target || !this.moving()) return false;
    const step = directorWalkStep(this.keys, this.yawDeg(), DIRECTOR_WALK_SPEED, dt);
    if (step.x !== 0 || step.y !== 0 || step.z !== 0) {
      this.target.position.x += step.x;
      this.target.position.y += step.y;
      this.target.position.z += step.z;
      this.moved = true;
    }
    return true;
  }

  private moving(): boolean {
    return MOVE_KEYS.some((k) => this.keys.has(k));
  }

  private release(): void {
    this.host.setReason("walk", false);
    if (this.moved) this.schedule();
  }

  private schedule(): void {
    this.cancelTimer();
    this.timer = setTimeout(() => {
      this.timer = null;
      this.flush();
    }, SETTLE_MS);
  }

  private cancelTimer(): void {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
  }

  /** 有未上报的移动就立刻上报一次。 */
  private flush(): void {
    this.cancelTimer();
    if (!this.moved) return;
    this.moved = false;
    this.onSettled();
  }

  /** 当前机位的偏航角（度）：0 = 朝 −Z 看，绕 +Y 逆时针为正（与 `directorWalkStep` 同口径）。 */
  private yawDeg(): number {
    if (!this.target) return 0;
    _forward.set(0, 0, -1).applyQuaternion(this.target.quaternion);
    return Math.atan2(-_forward.x, -_forward.z) / DEG;
  }

  private pitchDeg(): number {
    if (!this.target) return 0;
    _forward.set(0, 0, -1).applyQuaternion(this.target.quaternion);
    return Math.asin(Math.max(-1, Math.min(1, _forward.y))) / DEG;
  }

  private readonly onPointerDown = (event: PointerEvent) => {
    if (!this.enabled || !this.target || event.button !== 0 || this.drag) return;
    this.cancelTimer();
    this.drag = { id: event.pointerId, x: event.clientX, y: event.clientY, yaw: this.yawDeg(), pitch: this.pitchDeg() };
    this.canvas.setPointerCapture(event.pointerId);
    this.canvas.addEventListener("pointermove", this.onPointerMove);
    this.canvas.addEventListener("pointerup", this.onPointerUp);
    this.canvas.addEventListener("pointercancel", this.onPointerUp);
  };

  private readonly onPointerMove = (event: PointerEvent) => {
    const drag = this.drag;
    if (!drag || event.pointerId !== drag.id || !this.target) return;
    const yaw = drag.yaw - (event.clientX - drag.x) * TURN_DEG_PER_PX;
    const pitch = Math.max(-89, Math.min(89, drag.pitch - (event.clientY - drag.y) * TURN_DEG_PER_PX));
    this.target.quaternion.setFromEuler(_euler.set(pitch * DEG, yaw * DEG, 0, "YXZ"));
    this.moved = true;
    this.host.invalidate();
  };

  private readonly onPointerUp = (event: PointerEvent) => {
    if (!this.drag || event.pointerId !== this.drag.id) return;
    this.endDrag();
    if (this.moved && !this.moving()) this.schedule();
  };

  private endDrag(): void {
    const drag = this.drag;
    if (!drag) return;
    this.drag = null;
    if (this.canvas.hasPointerCapture(drag.id)) this.canvas.releasePointerCapture(drag.id);
    this.canvas.removeEventListener("pointermove", this.onPointerMove);
    this.canvas.removeEventListener("pointerup", this.onPointerUp);
    this.canvas.removeEventListener("pointercancel", this.onPointerUp);
  }

  private readonly onBlur = () => this.clear();

  private readonly onVisibility = () => {
    if (document.visibilityState === "hidden") this.clear();
  };
}
