/**
 * 变换手柄（DR-F-30 / 31 / 32 / 33 / 34，dev/03 §4.2 TransformTool）：three 的 TransformControls。
 *
 * - 手柄整体放 layer 1（`traverse` 到每个子物体）；它共用的射线也要开 layer 1，否则手柄点不中；
 * - 角色与机位不许缩放（DR-F-33）：切到 scale 时回到 translate 并通知 `onRejected`（界面提示「角色请用身高调整」）；
 *   道具缩放三轴可分；
 * - 网格吸附在**父坐标系**（stage 局部）里自己取整，不用 TransformControls 自带的 `translationSnap`：
 *   它在 world 空间取整，场景整体变换不是单位变换时，存进场景的局部坐标就不是步长的整数倍了（DR-AC-04）；
 *   角度吸附用自带的 `setRotationSnap`（相对拖动起点按步长转）；
 * - 地面吸附（DR-F-32）：水平移动时（拖动轴含 X 或 Z）每次 `objectChange` 都贴；只拖 Y 轴时不贴 ——
 *   那是用户明确要改高度，贴地的话 Y 手柄就成了摆设；
 * - 拖拽中只改 three 对象，不写 store；结束时通知 `onEnd` 一次（DR-F-34），由引擎发 `transform-end`。
 *
 * ⚠️ 拖拽时禁用 camera-controls，并加入连续渲染原因（DRD-35）；结束后撤掉，否则「静止零渲染」会红。
 */
import type { Camera, Object3D, Vector4 } from "three";
import { TransformControls } from "three/addons/controls/TransformControls.js";
import type { DirectorObjectKind } from "~/utils/directorScene";
import { DEG, type EngineContext } from "./context";
import type { DirectorTransformMode } from "./events";
import type { GroundSnap } from "./GroundSnap";
import { directorSetLayer, LAYER_HELPERS } from "./layers";
import type { RendererHost } from "./RendererHost";

/**
 * 手柄挂在谁身上。`joint`（DRT-21 关节模式）：`root` 是骨骼本身（旋转）或关节手柄空物体（手脚 IK、骨盆移动）；
 * 关节不做网格 / 地面吸附，旋转用局部坐标轴（转关节时按骨骼自己的轴更好理解）。
 */
export type TransformTarget = {
  id: string;
  kind: DirectorObjectKind;
  root: Object3D;
  joint?: { bone: string; translatable: boolean };
};

export interface TransformToolDeps {
  /** 一次拖拽开始 / 结束（结束时对象可能没动）。 */
  onDragging(target: TransformTarget, dragging: boolean): void;
  /** 拖拽途中对象变了（吸附之后）。 */
  onChange(target: TransformTarget): void;
  onRejected(target: TransformTarget, mode: DirectorTransformMode): void;
}

export type TransformSnap = { grid: number | null; angle: number | null; ground: boolean };

export class TransformTool {
  readonly controls: TransformControls;
  private target: TransformTarget | null = null;
  private mode: DirectorTransformMode = "translate";
  private snap: TransformSnap = { grid: null, angle: null, ground: true };

  constructor(
    ctx: EngineContext,
    canvas: HTMLCanvasElement,
    camera: Camera,
    private readonly host: RendererHost,
    private readonly groundSnap: GroundSnap,
    private readonly deps: TransformToolDeps
  ) {
    const controls = new TransformControls(camera, canvas);
    controls.size = 0.85;
    controls.getRaycaster().layers.enable(LAYER_HELPERS);
    const helper = controls.getHelper();
    directorSetLayer(helper, LAYER_HELPERS);
    ctx.scene.add(helper);
    controls.addEventListener("dragging-changed", this.onDraggingChanged);
    controls.addEventListener("objectChange", this.onObjectChange);
    controls.addEventListener("change", this.onChange);
    this.controls = controls;
    ctx.disposer.defer(() => {
      controls.removeEventListener("dragging-changed", this.onDraggingChanged);
      controls.removeEventListener("objectChange", this.onObjectChange);
      controls.removeEventListener("change", this.onChange);
      controls.detach();
      helper.removeFromParent();
      controls.dispose();
      this.host.setReason("transform", false);
    });
  }

  current(): TransformTarget | null {
    return this.target;
  }

  isDragging(): boolean {
    return this.controls.dragging;
  }

  /** 挂到一个对象上（null = 摘下）。切到不能缩放的对象时，缩放模式回到移动并通知。 */
  attach(target: TransformTarget | null): void {
    if (this.target?.root === target?.root && this.target?.id === target?.id && this.target?.joint?.bone === target?.joint?.bone) return;
    this.target = target;
    if (!target) {
      this.controls.detach();
    } else {
      this.controls.attach(target.root);
      if (this.mode === "scale" && !canScale(target.kind) && !target.joint) this.reject(target, "scale");
      this.applyControlsMode();
    }
    this.host.invalidate();
  }

  /**
   * 手柄实际用的模式与坐标系：关节只有「转」，能移动的关节（骨盆、手脚）在移动模式下才是「移」（DRT-21）；
   * 对象照用户选的模式，坐标系是世界。用户选的模式（`mode`）不因为挂到关节上而改掉。
   */
  private applyControlsMode(): void {
    const joint = this.target?.joint;
    if (joint) {
      const mode = this.mode === "translate" && joint.translatable ? "translate" : "rotate";
      this.controls.setMode(mode);
      this.controls.setSpace(mode === "rotate" ? "local" : "world");
      return;
    }
    this.controls.setMode(this.mode);
    this.controls.setSpace("world");
  }

  getMode(): DirectorTransformMode {
    return this.mode;
  }

  /** 切换模式；返回实际生效的模式（角色 / 机位切 scale 时是 translate）。 */
  setMode(mode: DirectorTransformMode): DirectorTransformMode {
    if (mode === "scale" && this.target && !canScale(this.target.kind)) {
      // 关节没有缩放：静默保持原来的模式（不发 mode-rejected，那句提示是说角色的）
      if (!this.target.joint) this.reject(this.target, mode);
      return this.mode;
    }
    this.mode = mode;
    this.applyControlsMode();
    this.host.invalidate();
    return mode;
  }

  setSnap(snap: TransformSnap): void {
    this.snap = snap;
    this.controls.setRotationSnap(snap.angle ? snap.angle * DEG : null);
  }

  /** 导演视角与机位视角之间切换时换相机；机位视角下 `viewport` 是画幅框（左下角为原点，CSS 像素）。 */
  setView(camera: Camera, viewport: Vector4 | null): void {
    this.controls.camera = camera;
    this.controls.viewport = viewport;
    this.host.invalidate();
  }

  setEnabled(on: boolean): void {
    this.controls.enabled = on;
  }

  private reject(target: TransformTarget, mode: DirectorTransformMode): void {
    this.mode = "translate";
    this.controls.setMode("translate");
    this.deps.onRejected(target, mode);
  }

  private readonly onDraggingChanged = (event: { value: unknown }) => {
    const dragging = event.value === true;
    this.host.setReason("transform", dragging);
    const target = this.target;
    if (!target) return;
    // 抬起时 TransformControls 先置 dragging = false、后清 axis，这里还拿得到这次拖的是哪个轴。
    if (!dragging) this.applySnap(target);
    this.deps.onDragging(target, dragging);
  };

  private readonly onObjectChange = () => {
    const target = this.target;
    if (!target || !this.controls.dragging) return;
    this.applySnap(target);
    this.deps.onChange(target);
  };

  private readonly onChange = () => this.host.invalidate();

  /** 网格吸附（父坐标系里按步长取整）与地面吸附；缩放夹到场景允许的范围。 */
  private applySnap(target: TransformTarget): void {
    // 关节不吸附：骨盆、手脚贴到网格或地面上会把姿势扯歪（旋转的角度吸附由 TransformControls 自己做）
    if (target.joint) return;
    if (this.controls.mode === "scale") {
      // ⚠️ 拖过对象中心时 TransformControls 会给出负缩放（镜像），而场景校验只收 0.01~100：不夹的话这一步存不进去。
      const s = target.root.scale;
      s.set(clampScale(s.x), clampScale(s.y), clampScale(s.z));
      return;
    }
    if (this.controls.mode !== "translate") return;
    const axis = this.controls.axis ?? "";
    const p = target.root.position;
    const step = this.snap.grid;
    if (step) {
      if (axis.includes("X")) p.x = Math.round(p.x / step) * step;
      if (axis.includes("Y")) p.y = Math.round(p.y / step) * step;
      if (axis.includes("Z")) p.z = Math.round(p.z / step) * step;
    }
    const horizontal = axis.includes("X") || axis.includes("Z");
    if (this.snap.ground && horizontal && target.kind !== "camera") {
      const y = this.groundSnap.surfaceBelow(target.root);
      if (y !== null) p.y = y;
    }
  }
}

/** 场景里缩放的取值范围（与 `directorValidateScene` 的 scale 校验一致）。 */
const SCALE_MIN = 0.01;
const SCALE_MAX = 100;

function clampScale(v: number): number {
  return Number.isFinite(v) ? Math.min(SCALE_MAX, Math.max(SCALE_MIN, v)) : 1;
}

function canScale(kind: DirectorObjectKind): boolean {
  return kind === "prop";
}
