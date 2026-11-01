/**
 * 3D 导演台渲染引擎（门面，dev/03 §4，DRT-12 / 13）。
 *
 * 引擎只**投影** store 里的场景：`setScene(scene)` 按对象 ID 做差量同步，自己不持有场景数据。用户在视口里的操作
 * （拖动、走位、点选）结束时发事件，由 store 改数据、记一步撤销，再经 `setScene` 回灌（DRD-36）。
 *
 * ⚠️ 同一页面同时只有一个活的渲染器（DRK-15、DRD-34）：预览小窗用同一个渲染器的 scissor 视口，不开第二个 canvas；
 *    关闭编辑器时**一定**要 dispose，并 `forceContextLoss()` 主动交还上下文（RendererHost）。
 * ⚠️ 按需渲染（DRD-35）：画面不变就不渲染，`invalidate()` 合并到下一帧（RendererHost）。
 * ⚠️ 上下文丢失后这个 canvas 不能再用：重新加载编辑器要换一个新的 canvas 元素（`webglcontextlost` 之后
 *    同一个 canvas 上拿到的还是那个已丢失的上下文）。
 */
import { Box3, Box3Helper, Group, PerspectiveCamera, Quaternion, Scene, Sphere, Vector3, Vector4, type Camera, type Material, type Object3D, type ShaderMaterial, type Texture } from "three";
import type { DirectorRect } from "~/utils/directorCamera";
import { DIRECTOR_PREVIEW_FPS, directorCaptureSize, directorDepthRange, directorLetterbox, directorPreviewSize } from "~/utils/directorCamera";
import { DirectorCaptureError, directorRenderCapture, directorRenderPixels, type DirectorCaptureResult } from "./Capture";
import { directorRecordPreview, type DirectorPreviewResult } from "./Recorder";
import { directorDepthMaterial, directorDrawPose, directorRenderDepth, type DirectorPose2D } from "./ControlMaps";
import { ProposalOverlay } from "./ProposalOverlay";
import type { DirectorObject, DirectorObjectKind, DirectorScene, DirectorVec3 } from "~/utils/directorScene";
import { directorIsPickable } from "~/utils/directorScene";
import { DIRECTOR_OBJECT_DEFAULTS } from "~/utils/directorCatalog";
import { directorProjectObject, directorTimelineOf } from "~/utils/directorAnimation";
import { DIRECTOR_OPENPOSE_POINTS, directorJointTranslatable, directorOpenPoseFromJoints } from "~/utils/directorPose";
import { directorSplatOf } from "~/utils/directorSplat";
import { DIRECTOR_FPS } from "~/utils/directorTimeline";
import { DIRECTOR_STANDARD_BONES, type DirectorBoneRotation } from "~/utils/directorRig";
import { Backdrop, DIRECTOR_PANORAMA_MAX_WIDTH } from "./Backdrop";
import { Splats } from "./Splats";
import { CameraRig, WalkController } from "./CameraRig";
import { Characters } from "./Characters";
import { asObject, bool, DEG, num, round6, vec3, type EngineContext } from "./context";
import { Disposer } from "./Disposer";
import { DirectorEmitter, type DirectorEngineEvents, type DirectorTransformMode, type DirectorTransformPayload } from "./events";
import { GroundSnap } from "./GroundSnap";
import { DIRECTOR_IK_CHAINS, directorBoneOverrideOf, directorIsIkEnd, directorSetWorldQuaternion, directorSolveTwoBone } from "./Ik";
import { DIRECTOR_JOINT_PICK_PREFIX, Joints } from "./Joints";
import { LabelLayer } from "./LabelLayer";
import { directorSetLayer, HELPER_CYAN, HELPER_MAGENTA, LAYER_CONTENT, LAYER_HELPERS } from "./layers";
import { Lights } from "./Lights";
import { Picking } from "./Picking";
import { Props } from "./Props";
import { Crowds } from "./Crowds";
import { RendererHost } from "./RendererHost";
import { ShotCameras, type ShotCamera } from "./ShotCameras";
import { TransformTool, type TransformTarget } from "./TransformTool";

/** 编辑器注入的依赖：引擎本身不碰登录态（受保护媒体由编辑器按当前工作区取成 blob，DRD-40）。 */
export type DirectorEngineDeps = {
  fetchMediaBlob?: (mediaId: string) => Promise<Blob>;
};

export interface DirectorEngineLike {
  mount(canvas: HTMLCanvasElement, labels?: HTMLElement | null, deps?: DirectorEngineDeps): void;
  invalidate(): void;
  dispose(): void;
}

export type DirectorViewMode = "director" | "camera";

export type DirectorEngineDebugInfo = {
  geometries: number;
  textures: number;
  renders: number;
  contextLost: boolean;
  /** 全景纹理的实际像素尺寸（DR-N-05）；没有全景时为 null。 */
  panorama: { width: number; height: number } | null;
  viewMode: DirectorViewMode;
  viewCameraId: string | null;
  walking: boolean;
  /** 机位视角下画幅框在视口里的矩形。 */
  frameRect: DirectorRect | null;
  directorCamera: { position: DirectorVec3; direction: DirectorVec3; target: DirectorVec3 };
  selectedId: string | null;
  transformMode: DirectorTransformMode;
};

/** 调试钩子（DRT-31 与各卡的浏览器检查用）：只在开发构建、或 URL 带 `?directorDebug=1` 时挂。 */
export type DirectorDebugHook = {
  memory(): { geometries: number; textures: number };
  /** 收到过 `webglcontextlost`（关闭编辑器时由 `forceContextLoss` 触发，DR-AC-02）。 */
  readonly contextLost: boolean;
  readonly renders: number;
  /** 最近一次截图的像素尺寸与 layer 0 对象清单（DRT-17 填）。 */
  lastCapture: { width: number; height: number; objects: string[] } | null;
  engine: DirectorEngine;
  /** `renderer.info` 的 memory 与 render 两段（DRT-31）；释放后 memory 取释放完那一刻的计数，render 为 null。 */
  rendererInfo(): { memory: { geometries: number; textures: number }; render: { calls: number; triangles: number; points: number; lines: number; frame: number } | null };
  /** 机位对象的世界坐标；`"director"` 取导演视角；找不到为 null（DRT-31）。 */
  cameraWorldPosition(id: string): DirectorVec3 | null;
  /** 最近 1 秒画了几帧（按需渲染：静止时为 0，DRT-31 的帧率测量在拖动视角时读）。 */
  fps(): number;
  /** 辅助专用色（品红、青），截图里统计它们出现的次数应为 0（DR-AC-15）。 */
  readonly helperColors: readonly string[];
};

declare global {
  interface Window {
    __directorDebug?: DirectorDebugHook;
  }
}

function debugEnabled(): boolean {
  if (import.meta.dev) return true;
  try {
    return new URLSearchParams(window.location.search).get("directorDebug") === "1";
  } catch {
    return false;
  }
}

type Parts = {
  readonly host: RendererHost;
  readonly scene: Scene;
  readonly stage: Group;
  readonly disposer: Disposer;
  readonly labels: LabelLayer;
  readonly backdrop: Backdrop;
  readonly splats: Splats;
  readonly lights: Lights;
  readonly shots: ShotCameras;
  readonly characters: Characters;
  readonly props: Props;
  readonly crowds: Crowds;
  readonly rig: CameraRig;
  readonly walk: WalkController;
  readonly transform: TransformTool;
  readonly picking: Picking;
  readonly groundSnap: GroundSnap;
  readonly selectionBox: Box3Helper;
  readonly proposal: ProposalOverlay;
  readonly joints: Joints;
};

/** 脚底锁定的一只脚（DRT-21）：拖动开始时记下的世界位置与朝向，拖动途中每一帧把腿拉回来。 */
type FootPin = {
  side: "left" | "right";
  chain: readonly [string, string, string];
  target: Vector3;
  world: Quaternion;
};

/** 一次会改骨骼的拖动（关节模式，或锁着脚拖动角色）。`start` 是可能被改到的骨骼在开始时的局部旋转。 */
type PoseDrag = {
  id: string;
  /** rotate：转一个关节；ik：拖手脚末端；hips：拖骨盆（移动身体）；body：锁着脚拖动 / 转动角色本身。 */
  kind: "rotate" | "ik" | "hips" | "body";
  bone: string | null;
  rootStart: Vector3;
  handleStart: Vector3;
  pins: FootPin[];
  released: Set<"left" | "right">;
  start: Map<string, Quaternion>;
};

/** 腿伸直也够不着锁定点的容差（米）：超过就解锁这只脚，不把腿拉断（DRT-21 风险 5）。 */
const FOOT_RELEASE_DISTANCE = 0.01;
const STANDARD_BONES = new Set(DIRECTOR_STANDARD_BONES);
const ZERO_ROTATION: DirectorBoneRotation = { x: 0, y: 0, z: 0 };

/** 两个覆盖值是不是同一个旋转（度，容差 0.001°，与写回的精度一致）。 */
function sameRotation(a: DirectorBoneRotation, b: DirectorBoneRotation): boolean {
  return Math.abs(a.x - b.x) < 1e-3 && Math.abs(a.y - b.y) < 1e-3 && Math.abs(a.z - b.z) < 1e-3;
}

type Located = { kind: DirectorObjectKind; root: Object3D; object: DirectorObject; pick: Object3D };

const _box = new Box3();

/** 深度图的近远写进记录与文件名，保留两位小数（厘米）就够。 */
function round2(v: number): number {
  return Math.round(v * 100) / 100;
}
const _box2 = new Box3();
const _v = new Vector3();

function full(width: number, height: number): DirectorRect {
  return { x: 0, y: 0, width, height };
}

export class DirectorEngine implements DirectorEngineLike {
  private readonly events = new DirectorEmitter<DirectorEngineEvents>();
  private parts: Parts | null = null;
  private disposed = false;
  private scene: DirectorScene | null = null;
  private mode: DirectorViewMode = "director";
  private viewCameraId: string | null = null;
  private previewRect: DirectorRect | null = null;
  private previewCameraId: string | null = null;
  private selectedId: string | null = null;
  private editable = true;
  private dragStart: string | null = null;
  /** 正在进行的姿势拖动（关节模式，或锁着脚拖动角色，DRT-21）。 */
  private poseDrag: PoseDrag | null = null;
  /** 时间轴（DRT-24）：当前帧、是否在播、播放的起点（墙钟与帧号）、当前被轨道驱动的对象。 */
  private frame = 0;
  private playing = false;
  /** 深度图的覆盖材质（第一次截深度图时建，关闭时随 Disposer 释放，DRT-27）。 */
  private depthMaterial: ShaderMaterial | null = null;
  private playClock = { time: 0, frame: 0 };
  private animated = new Set<string>();
  private finalMemory: { geometries: number; textures: number } | null = null;
  private readyFired = false;
  private settling = false;
  private resolveReady: () => void = () => {};
  private readonly ready = new Promise<void>((resolve) => {
    this.resolveReady = resolve;
  });

  mount(canvas: HTMLCanvasElement, labels?: HTMLElement | null, deps?: DirectorEngineDeps): void {
    if (this.disposed) throw new Error("DirectorEngine 已释放，不能再次挂载");
    if (this.parts) return;
    const disposer = new Disposer();
    const host = new RendererHost(canvas, {
      beforeRender: (dt) => this.beforeRender(dt),
      render: () => this.render(),
      resize: (w, h) => this.resize(w, h),
      contextLost: () => this.events.emit("context-lost", undefined)
    });
    const scene = new Scene();
    const stage = new Group();
    stage.name = "stage";
    scene.add(stage);
    const ctx: EngineContext = { scene, stage, disposer, invalidate: () => host.invalidate() };
    const container = canvas.parentElement ?? document.body;

    const labelLayer = new LabelLayer(labels ?? null, disposer);
    const maxWidth = Math.min(DIRECTOR_PANORAMA_MAX_WIDTH, host.renderer.capabilities.maxTextureSize);
    const backdrop = new Backdrop(ctx, deps?.fetchMediaBlob ?? null, maxWidth);
    const lights = new Lights(ctx);
    const shots = new ShotCameras(ctx);
    const characters = new Characters(ctx, labelLayer);
    const props = new Props(ctx);
    const crowds = new Crowds(ctx);
    // 泼溅背景（DRT-28）：Spark 在场景里第一次出现泼溅时才加载；加载失败、点太稀之类的说明经 notice 事件交给外壳
    const splats = new Splats(ctx, host.renderer, deps?.fetchMediaBlob ?? null, (text) => this.events.emit("notice", { level: "error", text }));
    const groundSnap = new GroundSnap(stage, () => backdrop.groundHeight, () => props.meshes(), (x, z, y) => splats.surfaceAt(x, z, y));

    // ⚠️ 顺序：变换手柄先连 DOM，再连 camera-controls，最后是点选。按在手柄上时它先处理按下、关掉 camera-controls、
    //    通知点选「这一下不算」；反过来的话同一次按下会先转动视角。CameraRig 构造时不连 DOM，`connect` 才连。
    const rig = new CameraRig(host, container, disposer);
    const transform = new TransformTool(ctx, canvas, rig.camera, host, groundSnap, {
      onDragging: (target, dragging) => this.onDragging(target, dragging),
      onChange: (target) => this.onTransformChange(target),
      onRejected: (target, mode) => this.events.emit("mode-rejected", { id: target.id, kind: target.kind, mode })
    });
    rig.connect(canvas);
    const picking = new Picking(
      canvas,
      {
        camera: () => this.viewCamera(),
        frame: () => this.mainRect(),
        targets: () => this.pickTargets(),
        onPick: (id) => this.pickFromViewport(id)
      },
      disposer
    );
    transform.controls.addEventListener("mouseDown", () => picking.suppress());
    const walk = new WalkController(host, canvas, disposer, () => this.emitCameraMoved());

    const selectionBox = new Box3Helper(new Box3(), HELPER_CYAN);
    selectionBox.visible = false;
    directorSetLayer(selectionBox, LAYER_HELPERS);
    scene.add(selectionBox);
    disposer.geometry(selectionBox.geometry);
    disposer.material(selectionBox.material as Material);
    const proposal = new ProposalOverlay(scene, disposer);
    const joints = new Joints(ctx);

    this.parts = {
      host,
      scene,
      stage,
      disposer,
      labels: labelLayer,
      backdrop,
      splats,
      lights,
      shots,
      characters,
      props,
      crowds,
      rig,
      walk,
      transform,
      picking,
      groundSnap,
      selectionBox,
      proposal,
      joints
    };
    if (debugEnabled()) this.installDebugHook();
    host.start();
  }

  // ---------------------------------------------------------------- 事件与就绪

  on<K extends keyof DirectorEngineEvents>(type: K, handler: (payload: DirectorEngineEvents[K]) => void): () => void {
    return this.events.on(type, handler);
  }

  /** 第一份场景（含素模）加载完毕。 */
  whenReady(): Promise<void> {
    return this.ready;
  }

  // ---------------------------------------------------------------- 场景投影

  setScene(scene: DirectorScene): void {
    const p = this.parts;
    if (!p) return;
    this.scene = scene;
    const defaults = DIRECTOR_OBJECT_DEFAULTS.scene;

    const stage = asObject(scene.stage);
    const offset = vec3(stage.offset, defaults.stage.offset);
    const rotation = vec3(stage.rotation, defaults.stage.rotation);
    p.stage.position.set(offset.x, offset.y, offset.z);
    p.stage.rotation.set(rotation.x * DEG, rotation.y * DEG, rotation.z * DEG, "XYZ");
    p.stage.scale.setScalar(Math.max(0.01, num(stage.scale, defaults.stage.scale)));

    const view = asObject(scene.view);
    p.labels.setShown(bool(view.showLabels, defaults.view.showLabels));
    const grid = asObject(view.gridSnap);
    const angle = asObject(view.angleSnap);
    p.transform.setSnap({
      grid: bool(grid.enabled, defaults.view.gridSnap.enabled) ? num(grid.step, defaults.view.gridSnap.step) : null,
      angle: bool(angle.enabled, defaults.view.angleSnap.enabled) ? num(angle.step, defaults.view.angleSnap.step) : null,
      ground: bool(view.groundSnap, defaults.view.groundSnap)
    });

    p.backdrop.apply(scene.background, scene.ground);
    p.splats.apply(directorSplatOf(scene));
    p.lights.apply(scene.lights);

    const seen = new Set<string>();
    const dragging = p.transform.isDragging() ? p.transform.current()?.id : undefined;
    const walking = p.walk.busyTarget();
    // 有轨道的对象交给下面的 applyTimeline 投影成当前帧的样子：这里先套基础值的话每次都要多摆一遍姿势
    const tracked = new Set(directorTimelineOf(scene).tracks.map((track) => track.objectId));
    for (const object of scene.objects ?? []) {
      if (!object || typeof object.id !== "string") continue;
      seen.add(object.id);
      // 正在拖的对象保持 three 上的临时位置，拖完由 transform-end → store → setScene 回灌。
      if (object.id === dragging) continue;
      this.removeIfKindChanged(object);
      if (tracked.has(object.id)) continue;
      if (object.kind === "character") p.characters.upsert(object);
      else if (object.kind === "prop") p.props.upsert(object);
      else if (object.kind === "camera") {
        const shot = p.shots.get(object.id);
        p.shots.upsert(object, !!shot && shot.root === walking);
      }
      else if (object.kind === "crowd") p.crowds.upsert(object);
    }
    for (const id of [...p.characters.ids(), ...p.props.ids(), ...p.crowds.ids(), ...p.shots.ids()]) {
      if (seen.has(id)) continue;
      p.characters.remove(id);
      p.props.remove(id);
      p.crowds.remove(id);
      p.shots.remove(id);
    }

    // 有轨道的对象按当前帧投影（DRT-24）；总时长缩短了的话播放头先夹回范围内
    this.frame = this.clampFrame(this.frame);
    this.applyTimeline();

    if (this.selectedId && !this.locate(this.selectedId)) this.selectedId = null;
    // 关节模式的角色被删了（撤销、远端更新）：关掉关节模式，外壳从选中状态里看得出来
    if (p.joints.id && !p.characters.get(p.joints.id)) p.joints.show(null, null, 1);
    if (this.pendingSelectId) {
      if (this.locate(this.pendingSelectId)) this.selectedId = this.pendingSelectId;
      this.pendingSelectId = null;
    }
    if (this.mode === "camera" && !p.shots.get(this.viewCameraId)) this.setViewMode({ mode: "director" });
    this.refreshTransformTarget();
    p.lights.fit(this.bounds(false));
    p.host.invalidate();
    if (!this.readyFired && !this.settling) void this.settleFirstScene();
  }

  private removeIfKindChanged(object: DirectorObject): void {
    const p = this.parts;
    if (!p) return;
    if (object.kind !== "character" && p.characters.get(object.id)) p.characters.remove(object.id);
    if (object.kind !== "prop" && p.props.get(object.id)) p.props.remove(object.id);
    if (object.kind !== "crowd" && p.crowds.get(object.id)) p.crowds.remove(object.id);
    if (object.kind !== "camera" && p.shots.get(object.id)) p.shots.remove(object.id);
  }

  private async settleFirstScene(): Promise<void> {
    const p = this.parts;
    if (!p) return;
    this.settling = true;
    await p.characters.whenIdle();
    await p.crowds.whenIdle();
    if (this.disposed || this.parts !== p) return;
    // 姿势库加载完之前投影的姿势轨道缺预设骨骼：重新投影一次
    this.applyTimeline();
    await p.characters.whenIdle();
    if (this.disposed || this.parts !== p) return;
    p.lights.fit(this.bounds(false));
    if (!p.rig.touched) p.rig.resetView(this.bounds(true), false);
    this.readyFired = true;
    this.settling = false;
    p.host.invalidate();
    this.resolveReady();
    this.events.emit("ready", undefined);
  }

  /** 找到一个对象在引擎里的投影。 */
  private locate(id: string): Located | null {
    const p = this.parts;
    if (!p) return null;
    const c = p.characters.get(id);
    if (c) return { kind: "character", root: c.root, object: c.object, pick: c.proxy };
    const prop = p.props.get(id);
    if (prop) return { kind: "prop", root: prop.root, object: prop.object, pick: prop.mesh };
    const crowd = p.crowds.get(id);
    if (crowd) return { kind: "crowd", root: crowd.root, object: crowd.object, pick: crowd.proxy };
    const shot = p.shots.get(id);
    if (shot) return { kind: "camera", root: shot.root, object: shot.object, pick: shot.body };
    return null;
  }

  /** 全部可见对象的包围球（世界坐标）。`withCameras` 为假时只算画面内容（灯光的阴影范围用）。 */
  private bounds(withCameras: boolean): Sphere {
    const p = this.parts;
    const box = new Box3();
    if (p) {
      p.scene.updateMatrixWorld();
      const ids = [...p.characters.ids(), ...p.props.ids(), ...p.crowds.ids(), ...(withCameras ? p.shots.ids() : [])];
      for (const id of ids) {
        const located = this.locate(id);
        if (!located || located.object.visible === false) continue;
        box.union(_box.setFromObject(located.pick));
      }
    }
    if (box.isEmpty()) box.set(new Vector3(-1, 0, -1), new Vector3(1, 1.8, 1));
    return box.getBoundingSphere(new Sphere());
  }

  // ---------------------------------------------------------------- 视角

  setViewMode(view: { mode: DirectorViewMode; cameraId?: string | null }): void {
    const p = this.parts;
    if (!p) return;
    let shot: ShotCamera | undefined;
    if (view.mode === "camera") {
      shot = p.shots.get(view.cameraId) ?? p.shots.get(this.scene?.outputCameraId ?? null) ?? p.shots.list()[0];
    }
    if (!shot) {
      if (p.walk.isEnabled()) p.walk.setEnabled(false, null);
      this.mode = "director";
      this.viewCameraId = null;
      p.rig.setActive(true);
      p.transform.setView(p.rig.camera, null);
    } else {
      const switching = this.viewCameraId !== shot.id;
      if (switching && p.walk.isEnabled()) p.walk.setEnabled(true, shot.root);
      this.mode = "camera";
      this.viewCameraId = shot.id;
      p.rig.setActive(false);
      p.transform.setView(shot.camera, this.transformViewport());
    }
    this.refreshTransformTarget();
    p.host.invalidate();
  }

  getViewMode(): { mode: DirectorViewMode; cameraId: string | null } {
    return { mode: this.mode, cameraId: this.viewCameraId };
  }

  /** 导演视角回到能看全全部可见对象的位置（DR-F-21）；机位视角下不动。 */
  resetView(): void {
    const p = this.parts;
    if (!p || this.mode !== "director") return;
    p.rig.resetView(this.bounds(true));
  }

  /** 导演视角聚焦一个对象（F 键）。 */
  focusObject(id: string): void {
    const p = this.parts;
    const located = this.locate(id);
    if (!p || !located || this.mode !== "director") return;
    p.scene.updateMatrixWorld();
    p.rig.focus(_box.setFromObject(located.pick).getBoundingSphere(new Sphere()));
  }

  /** 当前导演视角的位置与朝向（「用当前导演视角新建机位」，DR-F-26）。 */
  getDirectorView(): DirectorTransformPayload {
    const cam = this.parts?.rig.camera;
    const r = cam?.rotation;
    return {
      position: { x: round6(cam?.position.x ?? 0), y: round6(cam?.position.y ?? 0), z: round6(cam?.position.z ?? 0) },
      rotation: { x: round6((r?.x ?? 0) / DEG), y: round6((r?.y ?? 0) / DEG), z: round6((r?.z ?? 0) / DEG) },
      scale: { x: 1, y: 1, z: 1 }
    };
  }

  /** 对象包围盒的中心（世界坐标；「机位对准选中对象」，DR-F-26）。 */
  getObjectCenter(id: string): DirectorVec3 | null {
    const p = this.parts;
    const located = this.locate(id);
    if (!p || !located) return null;
    p.scene.updateMatrixWorld();
    const c = _box.setFromObject(located.pick).getCenter(_v);
    return { x: round6(c.x), y: round6(c.y), z: round6(c.z) };
  }

  // ---------------------------------------------------------------- AI 提案预览（DRT-20）

  private proposalActive = false;
  /** `select` 时引擎里还没有的对象（见 `select`）：下一次 `setScene` 出现了就选上。 */
  private pendingSelectId: string | null = null;
  private proposalToken = 0;

  /**
   * 预览 AI 搭景的建议场景（DR-F-102）：引擎临时投影建议场景，新增的对象绿框、修改的琥珀框、被删除的对象留红色虚影。
   * 被删除的对象要在切换**之前**按当前场景量包围盒（切过去它们就不在了）；新增 / 修改的等素模与群众烘焙完再量。
   * 预览期间视口里点不了；变换手柄由外壳经 `setEditable(false)` 撤掉。结束预览用 `clearProposal` + `setScene(当前场景)`。
   */
  setProposal(scene: DirectorScene, changes: { added: readonly string[]; updated: readonly string[]; removed: readonly string[] }): void {
    const p = this.parts;
    if (!p) return;
    p.scene.updateMatrixWorld();
    const boxOf = (id: string) => {
      const located = this.locate(id);
      return located ? new Box3().setFromObject(located.pick) : null;
    };
    const removed = changes.removed.map(boxOf).filter((box): box is Box3 => Boolean(box));
    const token = ++this.proposalToken;
    this.proposalActive = true;
    this.select(null);
    this.setScene(scene);
    void Promise.all([p.characters.whenIdle(), p.crowds.whenIdle()]).then(() => {
      if (this.parts !== p || !this.proposalActive || token !== this.proposalToken) return;
      p.scene.updateMatrixWorld();
      const added = changes.added.map(boxOf).filter((box): box is Box3 => Boolean(box));
      const updated = changes.updated.map(boxOf).filter((box): box is Box3 => Boolean(box));
      p.proposal.show({ added, updated, removed });
      p.host.invalidate();
    });
  }

  clearProposal(): void {
    this.proposalActive = false;
    this.proposalToken++;
    this.parts?.proposal.clear();
    this.parts?.host.invalidate();
  }

  /** 预览标记的个数（调试与检查脚本用）。 */
  proposalMarks(): number {
    return this.parts?.proposal.count ?? 0;
  }

  // ---------------------------------------------------------------- 时间轴（DRT-24，DR-F-120~124）

  /**
   * 跳到第 `frame` 帧：按轨道采样投影到对象上（只投影，不写场景）。帧号夹在 [0, 总时长] 并取整。
   * 导出预演（DRT-25）逐帧调它再截图：帧号决定画面，不依赖墙钟（DR-N-07）。
   */
  seek(frame: number): void {
    const next = this.clampFrame(frame);
    if (next === this.frame) return;
    this.frame = next;
    if (this.playing) this.playClock = { time: performance.now(), frame: next };
    this.applyTimeline();
    this.parts?.host.invalidate();
  }

  getFrame(): number {
    return this.frame;
  }

  isPlaying(): boolean {
    return this.playing;
  }

  /** 开始播放：帧号按墙钟推进（`floor(经过秒数 × 30)`）；停在末尾时从头播。只有播放时才连续渲染（DRD-35）。 */
  play(): void {
    const p = this.parts;
    if (!p || this.playing) return;
    if (this.frame >= this.duration()) {
      this.frame = 0;
      this.applyTimeline();
    }
    this.playing = true;
    this.playClock = { time: performance.now(), frame: this.frame };
    p.host.setReason("timeline", true);
    this.events.emit("frame", { frame: this.frame, playing: true });
  }

  /** 暂停：停在哪一帧，视口就显示哪一帧（撤掉连续渲染，否则整页风扇一直转）。 */
  pause(): void {
    if (!this.playing) return;
    this.playing = false;
    this.parts?.host.setReason("timeline", false);
    this.events.emit("frame", { frame: this.frame, playing: false });
  }

  private duration(): number {
    return directorTimelineOf(this.scene).durationFrames;
  }

  private clampFrame(frame: number): number {
    return Math.min(this.duration(), Math.max(0, Math.round(Number.isFinite(frame) ? frame : 0)));
  }

  /** 播放中每次渲染前推进帧号：过了末尾时，循环就从 0 接着播（带上超出的部分），否则停在最后一帧。 */
  private advancePlayback(): void {
    if (!this.playing) return;
    const duration = this.duration();
    const now = performance.now();
    let frame = this.playClock.frame + Math.floor(((now - this.playClock.time) / 1000) * DIRECTOR_FPS);
    if (frame > duration) {
      if (!directorTimelineOf(this.scene).loop) {
        this.frame = duration;
        this.applyTimeline();
        this.pause();
        return;
      }
      frame %= duration + 1;
      this.playClock = { time: now - (frame / DIRECTOR_FPS) * 1000, frame: 0 };
    }
    if (frame === this.frame) return;
    this.frame = frame;
    this.applyTimeline();
    this.events.emit("frame", { frame, playing: true });
  }

  /** 有轨道的对象投影成当前帧的样子；不再有轨道的对象恢复成场景里的基础值。正在拖的对象不动。 */
  private applyTimeline(): void {
    const p = this.parts;
    const scene = this.scene;
    if (!p || !scene) return;
    const tracks = directorTimelineOf(scene).tracks;
    const dragging = p.transform.isDragging() ? p.transform.current()?.id : undefined;
    const animated = new Set<string>();
    if (tracks.length) {
      const ids = new Set(tracks.map((track) => track.objectId));
      const poses = p.characters.poseLookup();
      for (const object of scene.objects ?? []) {
        if (!object || !ids.has(object.id)) continue;
        animated.add(object.id);
        if (object.id !== dragging) this.upsertObject(directorProjectObject(object, tracks, this.frame, poses));
      }
    }
    for (const id of this.animated) {
      if (animated.has(id)) continue;
      const object = scene.objects?.find((o) => o.id === id);
      if (object) this.upsertObject(object);
    }
    const changed = animated.size > 0 || this.animated.size > 0;
    this.animated = animated;
    if (changed) this.updateSelectionBox();
  }

  /** 按类型交给对应模块（setScene 的同一套；正在走位的机位保留 three 上的位置）。 */
  private upsertObject(object: DirectorObject): void {
    const p = this.parts as Parts;
    if (object.kind === "character") p.characters.upsert(object);
    else if (object.kind === "prop") p.props.upsert(object);
    else if (object.kind === "crowd") p.crowds.upsert(object);
    else if (object.kind === "camera") {
      const shot = p.shots.get(object.id);
      p.shots.upsert(object, !!shot && shot.root === p.walk.busyTarget());
    }
  }

  /** 角色是不是由姿势轨道驱动着（这时视口里的姿势是采样值，不是场景里的基础姿势）。 */
  private posedByTimeline(id: string): boolean {
    return this.animated.has(id) && directorTimelineOf(this.scene).tracks.some((track) => track.objectId === id && track.channel === "pose");
  }

  // ---------------------------------------------------------------- 姿势缩略图（DRT-21，开发态工具）

  /**
   * 给一个对象单独拍一张方形小图：离屏、只画 layer 0、透明背景，从对象前斜上方按包围球取景。场景与视角都不改。
   * 只给开发态的「生成姿势缩略图」工具用（`director/dev/DirectorSandbox.vue`）：缩略图生成后作为静态文件提交。
   */
  async renderThumbnail(id: string, options: { size: number; azimuth?: number; elevation?: number; mime?: "image/png" | "image/webp"; quality?: number }): Promise<Blob> {
    const p = this.parts;
    if (!p) throw new DirectorCaptureError("not_mounted", "编辑器还没准备好");
    await this.whenReady();
    await p.characters.whenIdle();
    await p.crowds.whenIdle();
    const located = this.locate(id);
    if (!located) throw new Error(`对象 ${id} 不在场景里`);
    p.scene.updateMatrixWorld(true);
    const sphere = new Box3().setFromObject(located.pick).getBoundingSphere(new Sphere());
    const fov = 30;
    const camera = new PerspectiveCamera(fov, 1, 0.05, 200);
    const azimuth = (options.azimuth ?? 25) * DEG;
    const elevation = (options.elevation ?? 10) * DEG;
    const distance = (Math.max(0.2, sphere.radius) / Math.sin((fov / 2) * DEG)) * 1.04;
    camera.position.set(
      sphere.center.x + distance * Math.cos(elevation) * Math.sin(azimuth),
      sphere.center.y + distance * Math.sin(elevation),
      sphere.center.z + distance * Math.cos(elevation) * Math.cos(azimuth)
    );
    camera.lookAt(sphere.center);
    camera.updateMatrixWorld(true);
    const result = await directorRenderCapture({
      renderer: p.host.renderer,
      scene: p.scene,
      camera,
      width: options.size,
      height: options.size,
      // 透明背景下抗锯齿的边缘会混进一点清屏色：取中灰，亮暗两套主题上都不显眼
      clearColor: 0x808080,
      clearAlpha: 0,
      mime: options.mime ?? "image/webp",
      quality: options.quality ?? 0.9
    });
    p.host.invalidate();
    return result.blob;
  }

  // ---------------------------------------------------------------- 截图（DRT-17）

  private readonly beforeCaptureHooks = new Set<() => Promise<void> | void>();

  /**
   * 截图前要等的事（泼溅排序 DRT-28 等）：返回取消登记的函数。素模与全景纹理的加载由引擎自己等。
   */
  addBeforeCapture(hook: () => Promise<void> | void): () => void {
    this.beforeCaptureHooks.add(hook);
    return () => this.beforeCaptureHooks.delete(hook);
  }

  /**
   * 截一个机位（DRD-39，dev/03 §7.1）：离屏、固定像素（画幅决定尺寸，DR-PARAM-08）、只画 layer 0。
   * 先等素模、全景纹理与登记的 `beforeCapture` 钩子：不等的话截到的是还没加载完的半成品。
   * 机位已不在场景里时抛 `DirectorCaptureError("camera_missing")`，界面据此给「改用输出机位」。
   */
  async capture(cameraId: string): Promise<DirectorCaptureResult> {
    const p = this.parts;
    if (!p) throw new DirectorCaptureError("not_mounted", "编辑器还没准备好");
    await this.whenReady();
    await p.characters.whenIdle();
    // 群众阵列的烘焙也是异步的（DRT-22）：刚加的阵列、刚换的姿势不等的话会截到旧样子或干脆缺一块
    await p.crowds.whenIdle();
    await p.backdrop.whenIdle();
    await p.splats.whenIdle();
    for (const hook of [...this.beforeCaptureHooks]) await hook();
    if (this.parts !== p) throw new DirectorCaptureError("not_mounted", "编辑器已关闭");
    const shot = p.shots.get(cameraId);
    if (!shot) throw new DirectorCaptureError("camera_missing", "该机位已删除");
    const size = directorCaptureSize(shot.aspectCode);
    // 泼溅按这个机位排好序再出图（DR-F-142）；放在最后一个 await：之后同步渲染，视口那一帧插不进来
    await this.splatsReady(p, shot.camera);
    // 透过的那个机位自己的模型本来就在 layer 1；这里照样藏一下，免得以后有人把机身挪进 layer 0
    const ownVisible = shot.model.visible;
    shot.model.visible = false;
    try {
      const result = await directorRenderCapture({
        renderer: p.host.renderer, scene: p.scene, camera: shot.camera, width: size.width, height: size.height, clearColor: p.backdrop.color
      });
      if (window.__directorDebug) window.__directorDebug.lastCapture = { width: result.width, height: result.height, objects: result.objects };
      return { blob: result.blob, width: result.width, height: result.height };
    } finally {
      shot.model.visible = ownVisible;
      p.host.invalidate();
    }
  }

  /** 泼溅按截图机位排序完成（DR-F-142）；等到上限也没排完时照常出图、只在控制台告警。 */
  private async splatsReady(p: Parts, camera: Camera): Promise<void> {
    if (!(await p.splats.ready(camera))) console.warn("[director] 泼溅排序超时，照常出图");
  }

  // ---------------------------------------------------------------- 预演视频（DRT-25，DR-F-125~129）

  /**
   * 导出预演：从第 0 帧到总时长逐帧离屏渲染这个机位（短边 720，`directorPreviewSize`），编码成 mp4（Recorder）。
   * 每一帧都先跳到那一帧再等素模、群众、背景与截图钩子 —— 与截图同一套等待，帧号决定画面（DR-N-07）。
   * 导出期间暂停播放；结束（完成、取消、出错）后回到原来那一帧。
   */
  async exportPreview(options: {
    cameraId: string;
    signal?: AbortSignal;
    onProgress?: (done: number, total: number) => void;
  }): Promise<DirectorPreviewResult> {
    const p = this.parts;
    if (!p) throw new DirectorCaptureError("not_mounted", "编辑器还没准备好");
    await this.whenReady();
    const shot = p.shots.get(options.cameraId);
    if (!shot) throw new DirectorCaptureError("camera_missing", "该机位已删除");
    const size = directorPreviewSize(shot.aspectCode);
    const frames = this.duration();
    const start = this.frame;
    this.pause();
    try {
      return await directorRecordPreview({
        width: size.width,
        height: size.height,
        fps: DIRECTOR_PREVIEW_FPS,
        frames,
        signal: options.signal,
        onProgress: options.onProgress,
        renderFrame: async (frame) => {
          this.frame = frame;
          this.applyTimeline();
          await p.characters.whenIdle();
          await p.crowds.whenIdle();
          await p.backdrop.whenIdle();
          await p.splats.whenIdle();
          for (const hook of [...this.beforeCaptureHooks]) await hook();
          if (this.parts !== p) throw new DirectorCaptureError("not_mounted", "编辑器已关闭");
          const current = p.shots.get(options.cameraId);
          if (!current) throw new DirectorCaptureError("camera_missing", "该机位已删除");
          await this.splatsReady(p, current.camera);
          const ownVisible = current.model.visible;
          current.model.visible = false;
          try {
            return directorRenderPixels({
              renderer: p.host.renderer, scene: p.scene, camera: current.camera, width: size.width, height: size.height, clearColor: p.backdrop.color
            });
          } finally {
            current.model.visible = ownVisible;
          }
        }
      });
    } finally {
      if (this.parts === p) {
        this.frame = start;
        this.applyTimeline();
        p.host.invalidate();
      }
    }
  }

  // ---------------------------------------------------------------- 控制图（DRT-27，DR-F-133 / 134）

  /**
   * 截一张控制图：深度图或 OpenPose 骨架图，与截图同机位、同尺寸、同一时刻；等待规则同截图（素模、群众、背景、钩子）。
   * - 深度图：near / far（米）取可见对象包围盒沿视线的最近 / 最远距离，前后各留 5%（`directorDepthRange`）；
   * - 骨架图：每个单人角色一套（群众是实例化渲染，没有逐人的骨骼，不出骨架，`crowds` 告诉界面要不要提示）。
   *   投影用渲染截图的同一个 three 相机，骨架与截图逐像素对得上（场景整体缩放过也一样）。
   */
  async captureControlMap(cameraId: string, kind: "depth" | "pose"): Promise<{
    blob: Blob; width: number; height: number; near?: number; far?: number; people?: DirectorPose2D[]; crowds?: boolean;
  }> {
    const p = this.parts;
    if (!p) throw new DirectorCaptureError("not_mounted", "编辑器还没准备好");
    await this.whenReady();
    await p.characters.whenIdle();
    await p.crowds.whenIdle();
    await p.backdrop.whenIdle();
    await p.splats.whenIdle();
    for (const hook of [...this.beforeCaptureHooks]) await hook();
    if (this.parts !== p) throw new DirectorCaptureError("not_mounted", "编辑器已关闭");
    const shot = p.shots.get(cameraId);
    if (!shot) throw new DirectorCaptureError("camera_missing", "该机位已删除");
    const size = directorCaptureSize(shot.aspectCode);
    const camera = shot.camera;
    const previousAspect = camera.aspect;
    camera.aspect = size.width / size.height;
    camera.updateProjectionMatrix();
    p.scene.updateMatrixWorld(true);
    try {
      if (kind === "depth") {
        const position = camera.getWorldPosition(new Vector3());
        const forward = new Vector3(0, 0, -1).applyQuaternion(camera.getWorldQuaternion(new Quaternion()));
        // 近远范围的规则只有一份（directorDepthRange，有单测）；一个可见对象都没有时给一个默认范围（整张接近黑）
        const range = directorDepthRange(this.contentBoxes(), { position, forward }) ?? { near: 0.1, far: 10 };
        this.depthMaterial ??= p.disposer.material(directorDepthMaterial()) as ShaderMaterial;
        const ownVisible = shot.model.visible;
        shot.model.visible = false;
        // 泼溅是半透明的高斯，没有可靠的深度，覆盖材质也画不了它：深度图里不含泼溅（dev/03 R-18）
        p.splats.setRendered(false);
        try {
          const blob = await directorRenderDepth({
            renderer: p.host.renderer, scene: p.scene, camera, material: this.depthMaterial,
            width: size.width, height: size.height, near: range.near, far: range.far
          });
          return { blob, width: size.width, height: size.height, near: round2(range.near), far: round2(range.far) };
        } finally {
          shot.model.visible = ownVisible;
          p.splats.setRendered(true);
        }
      }
      const people: DirectorPose2D[] = [];
      const bones = [...new Set([...DIRECTOR_OPENPOSE_POINTS.flatMap(point => (point.bone ? [point.bone] : [])), "Neck", "Head"])];
      const v = new Vector3();
      for (const id of p.characters.ids()) {
        const entry = p.characters.get(id);
        const skinned = p.characters.skinnedOf(id);
        if (!entry || !skinned || entry.object.visible === false) continue;
        const joints: Record<string, { x: number; y: number; z: number }> = {};
        for (const name of bones) {
          const bone = skinned.skeleton.getBoneByName(name);
          if (bone) {
            bone.getWorldPosition(v);
            joints[name] = { x: v.x, y: v.y, z: v.z };
          }
        }
        people.push(directorOpenPoseFromJoints(joints).map((point) => {
          if (!point) return null;
          const ndc = v.set(point.x, point.y, point.z).project(camera);
          const inFront = ndc.z >= -1 && ndc.z <= 1;
          if (!inFront || Math.abs(ndc.x) > 1 || Math.abs(ndc.y) > 1) return null;
          return { x: ((ndc.x + 1) / 2) * size.width, y: ((1 - ndc.y) / 2) * size.height };
        }));
      }
      const blob = await directorDrawPose(people, size.width, size.height);
      const crowds = p.crowds.ids().some(id => p.crowds.get(id)?.object.visible !== false);
      return { blob, width: size.width, height: size.height, people, crowds };
    } finally {
      camera.aspect = previousAspect;
      camera.updateProjectionMatrix();
      p.host.invalidate();
    }
  }

  /** 画面内容的包围盒（世界坐标）：可见的角色、道具、群众（机位与辅助不算）。 */
  private contentBoxes(): Array<{ min: Vector3; max: Vector3 }> {
    const p = this.parts as Parts;
    const out: Array<{ min: Vector3; max: Vector3 }> = [];
    const push = (object: Object3D | undefined, visible: boolean) => {
      if (!object || !visible) return;
      const box = new Box3().setFromObject(object);
      if (!box.isEmpty()) out.push({ min: box.min, max: box.max });
    };
    for (const id of p.characters.ids()) {
      const entry = p.characters.get(id);
      push(entry?.proxy, entry?.object.visible !== false);
    }
    for (const id of p.props.ids()) {
      const entry = p.props.get(id);
      push(entry?.mesh, entry?.object.visible !== false);
    }
    for (const id of p.crowds.ids()) {
      const entry = p.crowds.get(id);
      push(entry?.proxy, entry?.object.visible !== false);
    }
    return out;
  }

  /** 机位视角下画幅框在视口里的矩形（CSS 像素，左上角为原点）；导演视角下为 null。 */
  getFrameRect(): DirectorRect | null {
    return this.mode === "camera" ? this.mainRect() : null;
  }

  setPreviewRect(rect: DirectorRect | null): void {
    this.previewRect = rect ? { ...rect } : null;
    this.parts?.host.invalidate();
  }

  /** 预览小窗显示哪个机位；null = 输出机位（DR-F-22）。 */
  setPreviewCamera(id: string | null): void {
    this.previewCameraId = id;
    this.parts?.host.invalidate();
  }

  // ---------------------------------------------------------------- 自由走位

  /** 开关自由走位（只在机位视角下生效，DR-F-25）；返回走位是否开着。 */
  setWalk(on: boolean): boolean {
    const p = this.parts;
    if (!p) return false;
    const shot = this.mode === "camera" ? p.shots.get(this.viewCameraId) : undefined;
    p.walk.setEnabled(on && !!shot, shot?.root ?? null);
    this.refreshTransformTarget();
    return p.walk.isEnabled();
  }

  walkKey(code: string, down: boolean): void {
    this.parts?.walk.key(code, down);
  }

  clearWalkKeys(): void {
    this.parts?.walk.clear();
  }

  private emitCameraMoved(): void {
    const id = this.viewCameraId;
    const payload = id ? this.payloadOf(id) : null;
    if (!id || !payload) return;
    this.forceResync(id);
    this.events.emit("camera-moved", { id, transform: payload });
  }

  // ---------------------------------------------------------------- 选中与变换（DRT-13）

  /** 外部（场景树）选中一个对象；不发 `select` 事件。锁定、隐藏的对象可以被选中，但不挂变换手柄。 */
  /**
   * 选中一个对象。⚠️ 调用方经常**先** commit 再 select（场景树「添加」「复制」），而外壳的
   * `watch(store.scene) → setScene` 要等到下一拍才跑 —— 此时引擎里还没有这个对象。直接丢掉的话
   * 属性面板显示选中了、视口里却没有手柄也拖不动（DRT-22 发现）。所以认不出的 ID 先记下来，
   * 下一次 `setScene` 里出现了再选上；那一次还没有就作罢（它不会再来了）。
   */
  select(id: string | null): void {
    const known = Boolean(id && this.locate(id));
    this.pendingSelectId = id && !known ? id : null;
    this.selectedId = known ? id : null;
    this.refreshTransformTarget();
    this.parts?.host.invalidate();
  }

  getSelected(): string | null {
    return this.selectedId;
  }

  /** 切换变换模式（W / E / R）；返回实际生效的模式（角色、机位切缩放时是移动，并发 `mode-rejected`）。 */
  setTransformMode(mode: DirectorTransformMode): DirectorTransformMode {
    const p = this.parts;
    if (!p) return mode;
    const result = p.transform.setMode(mode);
    // 关节模式下「移」与「转」挂的不是同一个东西（手柄空物体 / 骨骼本身），要重挂
    if (p.joints.id) this.refreshTransformTarget();
    return result;
  }

  // ---------------------------------------------------------------- 关节模式（DRT-21，DR-F-43）

  /**
   * 打开（某个角色）/ 关闭关节模式。打开后视口里只能点关节小球，手柄挂在选中的关节上：
   * 「转」= 转这根骨骼（FK）；「移」只对骨盆（移动身体，锁定的脚留在原地）与手脚末端（IK）有效。
   */
  setJointMode(id: string | null): void {
    const p = this.parts;
    if (!p || p.transform.isDragging()) return;
    const valid = id && p.characters.get(id) ? id : null;
    const scale = valid ? (p.characters.get(valid)?.root.scale.x ?? 1) : 1;
    p.joints.show(valid, valid ? p.characters.skinnedOf(valid) : null, valid ? p.characters.heightOf(valid) * scale : 1.7);
    this.refreshTransformTarget();
    p.host.invalidate();
  }

  /** 选中一个关节（null = 不选）。不发 `joint-select`（那是视口点选才发的）。 */
  selectJoint(bone: string | null): void {
    const p = this.parts;
    if (!p || !p.joints.id || p.transform.isDragging()) return;
    p.joints.select(bone);
    this.refreshTransformTarget();
  }

  getJointMode(): { id: string | null; bone: string | null; joints: number } {
    const p = this.parts;
    return { id: p?.joints.id ?? null, bone: p?.joints.selectedBone ?? null, joints: p?.joints.count ?? 0 };
  }

  /** 骨骼当前的世界位置与世界朝向（调试与检查脚本用：IK 末端误差、脚底锁定前后的位置）。 */
  boneState(id: string, bone: string): { position: DirectorVec3; quaternion: [number, number, number, number] } | null {
    const p = this.parts;
    const b = p?.characters.skinnedOf(id)?.skeleton.getBoneByName(bone);
    if (!p || !b) return null;
    p.scene.updateMatrixWorld(true);
    const v = b.getWorldPosition(new Vector3());
    const q = b.getWorldQuaternion(new Quaternion());
    return { position: { x: v.x, y: v.y, z: v.z }, quaternion: [q.x, q.y, q.z, q.w] };
  }

  /** 骨骼当前相对 rest 的局部旋转（欧拉角，度）：检查脚本拿它与 `poses.json` 比（DR-AC-07）。 */
  boneOverride(id: string, bone: string): DirectorBoneRotation | null {
    const p = this.parts;
    const b = p?.characters.skinnedOf(id)?.skeleton.getBoneByName(bone);
    const rest = p?.characters.restOf(id, bone);
    return b && rest ? directorBoneOverrideOf(rest, b.quaternion) : null;
  }

  /** 只读时不挂变换手柄（DR-F-156）；点选照常。 */
  setEditable(on: boolean): void {
    this.editable = on;
    this.refreshTransformTarget();
  }

  /** 视口中心的视线与地面的交点（stage 局部坐标），给「添加对象」用（DR-F-11）；打不中回 null，调用方放原点。 */
  groundPointAtViewCenter(): DirectorVec3 | null {
    const p = this.parts;
    if (!p) return null;
    p.scene.updateMatrixWorld();
    return p.picking.groundPointAtCenter(p.stage, p.backdrop.groundHeight);
  }

  private pickTargets(): Object3D[] {
    const p = this.parts;
    if (!p) return [];
    // 关节模式下只能点关节小球：点到身体别处不该换选中的对象
    if (p.joints.id) return p.joints.pickTargets();
    const except = this.mode === "camera" ? this.viewCameraId : null;
    return [...p.characters.pickTargets(), ...p.props.pickTargets(), ...p.crowds.pickTargets(), ...p.shots.pickTargets(except)];
  }

  private pickFromViewport(id: string | null): void {
    // 提案预览期间视口里点不了（DR-F-102）：画面是建议场景，选中它的对象没有意义
    if (this.proposalActive) return;
    const p = this.parts;
    if (p?.joints.id) {
      const bone = id?.startsWith(DIRECTOR_JOINT_PICK_PREFIX) ? id.slice(DIRECTOR_JOINT_PICK_PREFIX.length) : null;
      if (bone === p.joints.selectedBone) return;
      p.joints.select(bone);
      this.refreshTransformTarget();
      this.events.emit("joint-select", { id: p.joints.id, bone: p.joints.selectedBone });
      return;
    }
    if (id === this.selectedId) return;
    this.select(id);
    this.events.emit("select", { id: this.selectedId, via: "viewport" });
  }

  /** 按当前状态决定变换手柄挂在谁身上。 */
  private refreshTransformTarget(): void {
    const p = this.parts;
    if (!p) return;
    const walking = p.walk.isEnabled();
    p.picking.enabled = !walking;
    p.transform.setEnabled(!walking && this.editable);
    if (p.joints.id) {
      this.refreshJointTarget(walking);
      return;
    }
    const located = this.selectedId ? this.locate(this.selectedId) : null;
    const attachable =
      located &&
      this.editable &&
      !walking &&
      directorIsPickable(located.object) &&
      !(this.mode === "camera" && this.selectedId === this.viewCameraId);
    const target: TransformTarget | null = attachable && this.selectedId ? { id: this.selectedId, kind: located.kind, root: located.root } : null;
    // 拖动途中不换手柄；但正在拖的对象被删掉了就得立刻摘下（否则手柄挂在一个已经不在场景里的对象上）。
    const current = p.transform.current();
    if (!p.transform.isDragging() || (current && !this.locate(current.id))) p.transform.attach(target);
    this.updateSelectionBox();
  }

  /** 关节模式下手柄挂在哪：选中的关节（转），或关节手柄空物体（骨盆与手脚末端的「移」）。 */
  private refreshJointTarget(walking: boolean): void {
    const p = this.parts as Parts;
    const id = p.joints.id as string;
    const bone = p.joints.selectedBone;
    const boneObject = p.joints.bone(bone);
    const located = this.locate(id);
    let target: TransformTarget | null = null;
    if (bone && boneObject && located && this.editable && !walking && directorIsPickable(located.object)) {
      const translatable = directorJointTranslatable(bone);
      const moving = translatable && p.transform.getMode() === "translate";
      // 拖动途中不挪手柄：refresh 也会在拖动时被调到（setScene 收尾），挪了的话这一下拖动就跳了
      if (moving && !p.transform.isDragging()) p.joints.placeHandle(boneObject);
      target = { id, kind: "character", root: moving ? p.joints.handle : boneObject, joint: { bone, translatable } };
    }
    if (!p.transform.isDragging()) p.transform.attach(target);
    this.updateSelectionBox();
  }

  private updateSelectionBox(): void {
    const p = this.parts;
    if (!p) return;
    const located = this.selectedId ? this.locate(this.selectedId) : null;
    const box = p.selectionBox;
    if (!located || located.object.visible === false || (this.mode === "camera" && this.selectedId === this.viewCameraId)) {
      box.visible = false;
      return;
    }
    located.root.updateWorldMatrix(true, true);
    box.box.copy(_box2.setFromObject(located.pick));
    box.visible = true;
    p.host.invalidate();
  }

  private onDragging(target: TransformTarget, dragging: boolean): void {
    const p = this.parts;
    if (!p) return;
    if (this.mode === "director") p.rig.controls.enabled = !dragging;
    if (dragging) {
      // 拖动时停下播放：边播边拖的话对象每一帧都被轨道拉回去
      this.pause();
      this.dragStart = JSON.stringify(this.payloadOf(target.id));
      this.poseDrag = this.startPoseDrag(target);
      return;
    }
    // 没有对应的「开始」就不算一次拖动结束：重复的结束信号会拿拖动途中的临时状态发 transform-end
    if (this.dragStart === null) return;
    const drag = this.poseDrag;
    this.poseDrag = null;
    if (drag) {
      const start = this.dragStart;
      this.dragStart = null;
      this.finishPoseDrag(drag, start);
      return;
    }
    const payload = this.payloadOf(target.id);
    const changed = payload !== null && JSON.stringify(payload) !== this.dragStart;
    this.dragStart = null;
    this.updateSelectionBox();
    // 落在了泼溅范围内、但脚下的点太稀，已退回地面（DR-F-141）：拖完说一次
    if (p.groundSnap.takeSplatFallback()) this.events.emit("notice", { level: "info", text: "这里的泼溅太稀，已贴到地面" });
    if (!payload || !changed) return;
    this.forceResync(target.id);
    this.events.emit("transform-end", { id: target.id, transform: payload });
  }

  private onTransformChange(target: TransformTarget): void {
    if (this.poseDrag) this.applyPoseDrag(this.poseDrag, target);
    this.updateSelectionBox();
  }

  // ---------------------------------------------------------------- 姿势拖动（DRT-21，DR-F-43，DR-AC-08）

  /**
   * 一次会改骨骼的拖动开始：关节模式下的任何拖动，或锁着脚拖动 / 转动角色本身。其余拖动返回 null（走 transform-end）。
   * 记下：可能被改到的骨骼的局部旋转（结束时据此判断改了哪几根）、root 与手柄的起点、锁定的脚的世界位置与朝向。
   */
  private startPoseDrag(target: TransformTarget): PoseDrag | null {
    const p = this.parts as Parts;
    const entry = p.characters.get(target.id);
    const skinned = p.characters.skinnedOf(target.id);
    if (target.kind !== "character" || !entry || !skinned) return null;
    const bone = target.joint?.bone ?? null;
    let kind: PoseDrag["kind"];
    if (!target.joint) kind = "body";
    else if (target.root !== p.joints.handle) kind = "rotate";
    else kind = bone === "Hips" ? "hips" : "ik";
    // 锁脚只在「身体动、脚不动」时生效：拖骨盆、转骨盆、拖动 / 转动角色本身
    const pins = kind === "body" || bone === "Hips" ? this.footPins(target.id) : [];
    if (kind === "body" && !pins.length) return null;
    const candidates = new Set<string>();
    if (kind === "rotate" && bone) candidates.add(bone);
    if (kind === "ik" && directorIsIkEnd(bone)) DIRECTOR_IK_CHAINS[bone].forEach((name) => candidates.add(name));
    for (const pin of pins) pin.chain.forEach((name) => candidates.add(name));
    const start = new Map<string, Quaternion>();
    for (const name of candidates) {
      const b = skinned.skeleton.getBoneByName(name);
      if (b) start.set(name, b.quaternion.clone());
    }
    return { id: target.id, kind, bone, rootStart: entry.root.position.clone(), handleStart: p.joints.handle.position.clone(), pins, released: new Set(), start };
  }

  /** 角色锁定了的脚（世界位置与朝向）。 */
  private footPins(id: string): FootPin[] {
    const p = this.parts as Parts;
    const entry = p.characters.get(id);
    const skinned = p.characters.skinnedOf(id);
    if (!entry || !skinned) return [];
    const base = (this.scene?.objects ?? []).find((o) => o.id === id) ?? entry.object;
    const lock = asObject(asObject(base.character).footLock);
    entry.root.updateMatrixWorld(true);
    const pins: FootPin[] = [];
    for (const side of ["left", "right"] as const) {
      if (lock[side] !== true) continue;
      const end = side === "left" ? "LeftFoot" : "RightFoot";
      const foot = skinned.skeleton.getBoneByName(end);
      if (!foot) continue;
      pins.push({ side, chain: DIRECTOR_IK_CHAINS[end], target: foot.getWorldPosition(new Vector3()), world: foot.getWorldQuaternion(new Quaternion()) });
    }
    return pins;
  }

  /** 拖动途中（每次 objectChange）：按拖动的种类改骨骼 / root，再把锁定的脚拉回原处。 */
  private applyPoseDrag(drag: PoseDrag, target: TransformTarget): void {
    const p = this.parts as Parts;
    const entry = p.characters.get(drag.id);
    const skinned = p.characters.skinnedOf(drag.id);
    if (!entry || !skinned) return;
    const forward = entry.root.getWorldDirection(new Vector3());
    if (drag.kind === "ik" && directorIsIkEnd(drag.bone)) {
      const [upper, lower, end] = DIRECTOR_IK_CHAINS[drag.bone].map((name) => skinned.skeleton.getBoneByName(name));
      // 膝盖朝前折、手肘朝后折（素模面朝 +Z）
      const hint = drag.bone.endsWith("Foot") ? forward : forward.clone().negate();
      if (upper && lower && end) directorSolveTwoBone(upper, lower, end, p.joints.handle.position, hint);
    } else if (drag.kind === "hips") {
      // 手柄在世界里挪了多少，root 在 stage 局部坐标里跟着挪多少（场景整体可能被缩放、旋转过）
      const from = p.stage.worldToLocal(drag.handleStart.clone());
      const to = p.stage.worldToLocal(p.joints.handle.position.clone());
      entry.root.position.copy(drag.rootStart).add(to.sub(from));
    }
    if (drag.pins.length) this.pullFeet(drag, skinned, forward);
    p.joints.update();
    p.host.invalidate();
    void target;
  }

  /** 把锁定的脚拉回原处并恢复脚掌朝向；腿伸直也够不着的那只解锁，腿回到拖动前的样子。 */
  private pullFeet(drag: PoseDrag, skinned: NonNullable<ReturnType<Characters["skinnedOf"]>>, forward: Vector3): void {
    const p = this.parts as Parts;
    p.characters.get(drag.id)?.root.updateMatrixWorld(true);
    for (const pin of drag.pins) {
      if (drag.released.has(pin.side)) continue;
      const [upper, lower, foot] = pin.chain.map((name) => skinned.skeleton.getBoneByName(name));
      if (!upper || !lower || !foot) continue;
      const error = directorSolveTwoBone(upper, lower, foot, pin.target, forward);
      if (error > FOOT_RELEASE_DISTANCE) {
        drag.released.add(pin.side);
        for (const name of pin.chain) {
          const original = drag.start.get(name);
          if (original) skinned.skeleton.getBoneByName(name)?.quaternion.copy(original);
        }
        continue;
      }
      directorSetWorldQuaternion(foot, pin.world);
    }
  }

  /** 拖动结束：把改到的骨骼换成覆盖，连同 root 的新位置（锁着脚时竖直方向由腿吸收）一起发 `pose-end`。 */
  private finishPoseDrag(drag: PoseDrag, startPayload: string | null): void {
    const p = this.parts as Parts;
    const entry = p.characters.get(drag.id);
    const skinned = p.characters.skinnedOf(drag.id);
    if (!entry || !skinned) return;
    const changed: string[] = [];
    for (const [name, original] of drag.start) {
      const bone = skinned.skeleton.getBoneByName(name);
      if (bone && bone.quaternion.angleTo(original) > 1e-6) changed.push(name);
    }
    let transform: DirectorTransformPayload | undefined;
    if (drag.kind === "hips" || drag.kind === "body") {
      const payload = this.payloadOf(drag.id);
      if (payload) {
        // 锁着脚（或拖的是骨盆）：root 的高度不变，身体的高低由腿弯多少决定 —— 落地规则（最低点贴 root）下，
        // 脚还在原处（DR-AC-08）；把竖直位移写进 root 的话，提交后脚会跟着沉下去或浮起来
        payload.position.y = round6(drag.rootStart.y);
        if (JSON.stringify(payload) !== startPayload) transform = payload;
      }
    }
    this.updateSelectionBox();
    if (!changed.length && !transform) {
      // 没有能写回的改动（只上下拖了骨盆而脚没锁、腿够不着全解锁了……）：root 回到原处，画面回到 store 里的样子
      entry.root.position.copy(drag.rootStart);
      this.forceResync(drag.id);
      if (this.scene) this.setScene(this.scene);
      return;
    }
    // 基础姿势取 store 的场景：有姿势轨道时引擎里的对象是投影出来的（骨骼是采样值）
    const base = (this.scene?.objects ?? []).find((o) => o.id === drag.id) ?? entry.object;
    const character = asObject(base.character);
    // 有姿势轨道时视口里是采样出来的姿势（可能在两个姿势之间，DRT-24）：覆盖要相对基础预设写全，
    // 只写拖到的那几根的话，没拖到的骨骼在提交后会跳回基础姿势
    const posed = this.posedByTimeline(drag.id);
    const overrides: Record<string, DirectorBoneRotation> = posed ? {} : { ...(asObject(character.boneOverrides) as Record<string, DirectorBoneRotation>) };
    const presetBones = posed ? (p.characters.poseLookup()(String(character.posePreset ?? ""))?.bones ?? {}) : {};
    const names = posed ? skinned.skeleton.bones.map((b) => b.name).filter((name) => STANDARD_BONES.has(name)) : changed;
    for (const name of names) {
      const bone = skinned.skeleton.getBoneByName(name);
      const rest = p.characters.restOf(drag.id, name);
      if (!bone || !rest) continue;
      const value = directorBoneOverrideOf(rest, bone.quaternion);
      if (posed && sameRotation(value, presetBones[name] ?? ZERO_ROTATION)) continue;
      overrides[name] = value;
    }
    const released = [...drag.released];
    const lock = asObject(character.footLock);
    this.forceResync(drag.id);
    this.events.emit("pose-end", {
      id: drag.id,
      boneOverrides: overrides,
      ...(transform ? { transform } : {}),
      ...(released.length ? { footLock: { left: lock.left === true && !released.includes("left"), right: lock.right === true && !released.includes("right") }, released } : {})
    });
  }

  /** 发出事件后，下一次 `setScene` 无论这个对象变没变都重新套用：store 若没采纳这次改动，对象回到 store 里的样子。 */
  private forceResync(id: string): void {
    const p = this.parts;
    if (!p) return;
    const entry = p.characters.get(id) ?? p.props.get(id) ?? p.crowds.get(id) ?? p.shots.get(id);
    if (entry) entry.signature = "";
  }

  /** 对象当前的变换（three 上的值，含拖动、走位途中的临时状态），换成场景口径。 */
  private payloadOf(id: string): DirectorTransformPayload | null {
    const located = this.locate(id);
    if (!located) return null;
    const { root, kind, object } = located;
    const original = vec3(asObject(object.transform).scale, { x: 1, y: 1, z: 1 });
    return {
      position: { x: round6(root.position.x), y: round6(root.position.y), z: round6(root.position.z) },
      rotation: { x: round6(root.rotation.x / DEG), y: round6(root.rotation.y / DEG), z: round6(root.rotation.z / DEG) },
      // 机位不缩放：原样带回场景里的值，不让引擎替它改。
      scale: kind === "camera" ? original : { x: round6(root.scale.x), y: round6(root.scale.y), z: round6(root.scale.z) }
    };
  }

  // ---------------------------------------------------------------- 渲染

  invalidate(): void {
    this.parts?.host.invalidate();
  }

  private viewCamera(): Camera {
    const p = this.parts as Parts;
    return this.mode === "camera" ? (p.shots.get(this.viewCameraId)?.camera ?? p.rig.camera) : p.rig.camera;
  }

  /** 主画面在视口里的矩形：导演视角是整个视口，机位视角是按画幅留黑边后的框（DR-AC-05）。 */
  private mainRect(): DirectorRect {
    const p = this.parts;
    if (!p) return full(0, 0);
    const shot = this.mode === "camera" ? p.shots.get(this.viewCameraId) : undefined;
    return shot ? directorLetterbox(p.host.width, p.host.height, shot.aspect) : full(p.host.width, p.host.height);
  }

  private transformViewport(): Vector4 | null {
    const p = this.parts;
    if (!p || this.mode !== "camera") return null;
    const f = this.mainRect();
    return new Vector4(f.x, p.host.height - f.y - f.height, f.width, f.height);
  }

  private resize(width: number, height: number): void {
    const p = this.parts;
    if (!p) return;
    p.rig.resize(width, height);
    if (this.mode === "camera") p.transform.setView(this.viewCamera(), this.transformViewport());
  }

  private beforeRender(dt: number): boolean {
    const p = this.parts;
    if (!p) return false;
    this.advancePlayback();
    // 关节小球跟着骨骼走；换了素模（克隆换了一份）时重建
    const jointId = p.joints.id;
    if (jointId) p.joints.show(jointId, p.characters.skinnedOf(jointId), p.characters.heightOf(jointId) * (p.characters.get(jointId)?.root.scale.x ?? 1));
    let moving = false;
    if (p.rig.update(dt)) moving = true;
    if (p.walk.update(dt)) moving = true;
    return moving;
  }

  private render(): void {
    const p = this.parts;
    if (!p) return;
    // 只在挂了调试钩子时记帧时间（DRT-31 的帧率测量），留最近 2 秒
    if (this.debugFrames) {
      const now = performance.now();
      this.debugFrames.push(now);
      while (this.debugFrames.length && (this.debugFrames[0] as number) < now - 2000) this.debugFrames.shift();
    }
    const r = p.host.renderer;
    const W = p.host.width;
    const H = p.host.height;
    // 一帧里可能渲染两次（主画面 + 预览），阴影贴图只在第一次更新。
    r.shadowMap.autoUpdate = false;
    r.shadowMap.needsUpdate = true;
    r.setScissorTest(false);
    r.setViewport(0, 0, W, H);

    const shot = this.mode === "camera" ? p.shots.get(this.viewCameraId) : undefined;
    const camera = shot?.camera ?? p.rig.camera;
    const rect = this.mainRect();
    if (shot) {
      // 黑边：先整块清黑，再在画幅框里清背景色。
      r.setClearColor(0x000000, 1);
      r.clear();
      this.setRegion(rect);
    }
    r.setClearColor(p.backdrop.color, 1);
    r.clear();
    camera.layers.set(LAYER_CONTENT);
    camera.layers.enable(LAYER_HELPERS);
    // 透过机位看时，这个机位自己的模型不画（否则画面正中是机身的内壁）。
    const ownModel = shot?.model;
    const ownVisible = ownModel?.visible ?? false;
    if (ownModel) ownModel.visible = false;
    if (this.selectedId) this.updateSelectionBoxQuiet();
    r.render(p.scene, camera);
    if (ownModel) ownModel.visible = ownVisible;
    r.setScissorTest(false);
    r.setViewport(0, 0, W, H);
    p.labels.render(p.scene, camera, rect);

    this.renderPreview();
    r.setScissorTest(false);
    r.setViewport(0, 0, W, H);
    if (!shot) p.rig.renderGizmo();
  }

  /** 机位预览小窗：同一个渲染器的 scissor 视口（DRD-34），只画 layer 0（与截图同口径）。 */
  private renderPreview(): void {
    const p = this.parts;
    const rect = this.previewRect;
    if (!p || !rect || rect.width < 2 || rect.height < 2) return;
    const shot = p.shots.get(this.previewCameraId) ?? p.shots.get(this.scene?.outputCameraId ?? null);
    if (!shot) return;
    const r = p.host.renderer;
    const box = { x: Math.round(rect.x), y: Math.round(rect.y), width: Math.round(rect.width), height: Math.round(rect.height) };
    const inner = directorLetterbox(box.width, box.height, shot.aspect);
    this.setRegion(box);
    r.setClearColor(0x000000, 1);
    r.clear();
    this.setRegion({ x: box.x + inner.x, y: box.y + inner.y, width: inner.width, height: inner.height });
    r.setClearColor(p.backdrop.color, 1);
    r.clear();
    shot.camera.layers.set(LAYER_CONTENT);
    r.render(p.scene, shot.camera);
    shot.camera.layers.enable(LAYER_HELPERS);
  }

  /** 视口 + 裁剪设到一个矩形（输入是左上角为原点的 CSS 像素，WebGL 要左下角为原点）。 */
  private setRegion(rect: DirectorRect): void {
    const p = this.parts as Parts;
    const r = p.host.renderer;
    const y = p.host.height - rect.y - rect.height;
    r.setViewport(rect.x, y, rect.width, rect.height);
    r.setScissor(rect.x, y, rect.width, rect.height);
    r.setScissorTest(true);
  }

  private updateSelectionBoxQuiet(): void {
    const p = this.parts;
    if (!p || !p.selectionBox.visible || !this.selectedId) return;
    const located = this.locate(this.selectedId);
    if (!located) return;
    // setFromObject 只刷新代理自己的世界矩阵，root 刚被拖动过的话要先把它刷新，否则选中框慢一帧。
    located.root.updateWorldMatrix(true, false);
    p.selectionBox.box.copy(_box2.setFromObject(located.pick));
  }

  // ---------------------------------------------------------------- 调试与释放

  debugInfo(): DirectorEngineDebugInfo {
    const p = this.parts;
    const memory = this.memory();
    const cam = p?.rig.camera;
    const dir = cam ? cam.getWorldDirection(new Vector3()) : new Vector3(0, 0, -1);
    const target = p ? p.rig.controls.getTarget(new Vector3()) : new Vector3();
    const v = (x: Vector3): DirectorVec3 => ({ x: round6(x.x), y: round6(x.y), z: round6(x.z) });
    return {
      ...memory,
      renders: (p?.host ?? this.lastHost)?.renders ?? 0,
      contextLost: (p?.host ?? this.lastHost)?.contextLost ?? false,
      panorama: p?.backdrop.textureSize() ?? null,
      viewMode: this.mode,
      viewCameraId: this.viewCameraId,
      walking: p?.walk.isEnabled() ?? false,
      frameRect: this.getFrameRect(),
      directorCamera: { position: v(cam?.position ?? new Vector3()), direction: v(dir), target: v(target) },
      selectedId: this.selectedId,
      transformMode: p?.transform.getMode() ?? "translate"
    };
  }

  /**
   * 调试用：只开 layer 0 用当前视角渲染一帧并读回像素，统计辅助专用色与变换手柄颜色出现的次数，
   * 同时列出 layer 0 可见的对象（DRT-13：手柄不进 layer 0）。读像素必须与渲染在同一个任务里。
   */
  debugLayer0Probe(includeHelpers = false): { magentaOrCyan: number; handleColors: number; layer0Objects: string[]; pixels: number } {
    const p = this.parts;
    if (!p) return { magentaOrCyan: 0, handleColors: 0, layer0Objects: [], pixels: 0 };
    const r = p.host.renderer;
    const camera = this.viewCamera();
    const mask = camera.layers.mask;
    const rect = this.mainRect();
    r.shadowMap.needsUpdate = true;
    this.setRegion(rect);
    r.setClearColor(p.backdrop.color, 1);
    r.clear();
    camera.layers.set(LAYER_CONTENT);
    // 对照组：同样的读法、把辅助层也画上，证明这套计数确实看得见手柄与品红机位模型。
    if (includeHelpers) camera.layers.enable(LAYER_HELPERS);
    r.render(p.scene, camera);
    camera.layers.mask = mask;
    r.setScissorTest(false);
    const gl = r.getContext();
    const dpr = r.getPixelRatio();
    const w = Math.max(1, Math.floor(rect.width * dpr));
    const h = Math.max(1, Math.floor(rect.height * dpr));
    const x = Math.floor(rect.x * dpr);
    const y = Math.floor((p.host.height - rect.y - rect.height) * dpr);
    const buf = new Uint8Array(w * h * 4);
    gl.readPixels(x, y, w, h, gl.RGBA, gl.UNSIGNED_BYTE, buf);
    let magentaOrCyan = 0;
    let handleColors = 0;
    for (let i = 0; i < buf.length; i += 4) {
      const R = buf[i] as number;
      const G = buf[i + 1] as number;
      const B = buf[i + 2] as number;
      if ((R > 235 && G < 25 && B > 235) || (R < 25 && G > 235 && B > 235)) magentaOrCyan++;
      // TransformControls 的轴色：纯红、纯绿、纯蓝、高亮黄。
      if ((R > 235 && G < 25 && B < 25) || (R < 25 && G > 235 && B < 25) || (R < 25 && G < 25 && B > 235) || (R > 235 && G > 235 && B < 25)) handleColors++;
    }
    const layer0Objects: string[] = [];
    p.scene.traverseVisible((o) => {
      if (o.layers.isEnabled(LAYER_CONTENT) && ((o as { isMesh?: boolean }).isMesh || (o as { isLine?: boolean }).isLine)) {
        layer0Objects.push(o.name || o.type);
      }
    });
    p.host.invalidate();
    return { magentaOrCyan, handleColors, layer0Objects, pixels: w * h };
  }

  /** 调试用：世界坐标点在视口里的位置（CSS 像素，相对 canvas 左上角）；在相机背后时为 null。 */
  debugProject(point: DirectorVec3): { x: number; y: number } | null {
    const p = this.parts;
    if (!p) return null;
    const camera = this.viewCamera() as Camera & { updateMatrixWorld(): void };
    p.scene.updateMatrixWorld();
    camera.updateMatrixWorld();
    const ndc = _v.set(point.x, point.y, point.z).project(camera);
    if (ndc.z < -1 || ndc.z > 1) return null;
    const rect = this.mainRect();
    return { x: rect.x + ((ndc.x + 1) / 2) * rect.width, y: rect.y + ((1 - ndc.y) / 2) * rect.height };
  }

  /** 调试用：把导演视角直接摆到某个位置看向某点（无过渡），检查脚本用来得到确定的视角。 */
  debugSetDirectorView(position: DirectorVec3, target: DirectorVec3): void {
    const p = this.parts;
    if (!p) return;
    void p.rig.controls.setLookAt(position.x, position.y, position.z, target.x, target.y, target.z, false);
    p.rig.touched = true;
    p.host.renderNow();
  }

  /** 调试用：变换手柄某个拾取块（如 `"XZ"`、`"XYZ"`、`"X"`）在视口里的位置（CSS 像素，相对 canvas 左上角）。 */
  debugHandlePoint(name: string): { x: number; y: number } | null {
    const p = this.parts;
    if (!p || !p.transform.current()) return null;
    const helper = p.transform.controls.getHelper();
    helper.updateMatrixWorld(true);
    let gizmo: { picker?: Record<string, Object3D> } | null = null;
    helper.traverse((o) => {
      if (!gizmo && (o as { picker?: unknown }).picker) gizmo = o as { picker?: Record<string, Object3D> };
    });
    const picker = (gizmo as { picker?: Record<string, Object3D> } | null)?.picker?.[p.transform.getMode()];
    const handle = picker?.children.find((c) => c.name === name);
    if (!handle) return null;
    const center = new Box3().setFromObject(handle).getCenter(new Vector3());
    const ndc = center.project(this.viewCamera());
    const rect = this.mainRect();
    return { x: rect.x + ((ndc.x + 1) / 2) * rect.width, y: rect.y + ((1 - ndc.y) / 2) * rect.height };
  }

  /** 调试用：对象在 three 上的当前变换（含拖动、走位途中的临时状态），场景口径。 */
  debugTransformOf(id: string): DirectorTransformPayload | null {
    return this.payloadOf(id);
  }

  /**
   * 调试用：同步渲染一帧（与平时同样的图层）并读回主画面的像素，给出平均亮度与 8×8 分块的平均色。
   * 读像素必须与渲染在同一个任务里：`preserveDrawingBuffer` 为假，画完缓冲区就会被交换。
   */
  debugCapturePixels(): { luminance: number; cells: number[]; width: number; height: number } {
    const p = this.parts;
    if (!p) return { luminance: 0, cells: [], width: 0, height: 0 };
    p.host.renderNow();
    const r = p.host.renderer;
    const gl = r.getContext();
    const dpr = r.getPixelRatio();
    const rect = this.mainRect();
    const w = Math.max(1, Math.floor(rect.width * dpr));
    const h = Math.max(1, Math.floor(rect.height * dpr));
    const x = Math.floor(rect.x * dpr);
    const y = Math.floor((p.host.height - rect.y - rect.height) * dpr);
    const buf = new Uint8Array(w * h * 4);
    gl.readPixels(x, y, w, h, gl.RGBA, gl.UNSIGNED_BYTE, buf);
    const N = 8;
    const sums = new Float64Array(N * N * 3);
    const counts = new Float64Array(N * N);
    let lum = 0;
    for (let row = 0; row < h; row++) {
      // readPixels 的第 0 行在底部；分块按屏幕方向（第 0 行在顶部）排。
      const cy = Math.min(N - 1, Math.floor(((h - 1 - row) / h) * N));
      for (let col = 0; col < w; col++) {
        const i = (row * w + col) * 4;
        const cx = Math.min(N - 1, Math.floor((col / w) * N));
        const R = buf[i] as number;
        const G = buf[i + 1] as number;
        const B = buf[i + 2] as number;
        const cell = cy * N + cx;
        sums[cell * 3] = (sums[cell * 3] as number) + R;
        sums[cell * 3 + 1] = (sums[cell * 3 + 1] as number) + G;
        sums[cell * 3 + 2] = (sums[cell * 3 + 2] as number) + B;
        counts[cell] = (counts[cell] as number) + 1;
        lum += 0.2126 * R + 0.7152 * G + 0.0722 * B;
      }
    }
    const cells: number[] = [];
    for (let c = 0; c < N * N; c++) {
      const n = Math.max(1, counts[c] as number);
      cells.push(Math.round((sums[c * 3] as number) / n), Math.round((sums[c * 3 + 1] as number) / n), Math.round((sums[c * 3 + 2] as number) / n));
    }
    return { luminance: lum / (w * h), cells, width: w, height: h };
  }

  /** 调试用：每个可见对象的包围盒 8 个角是否都在当前视锥里（DR-AC-05「重置视角后都在视锥内」）。 */
  debugFrustumCheck(): Array<{ id: string; inside: boolean }> {
    const p = this.parts;
    if (!p) return [];
    p.scene.updateMatrixWorld();
    const camera = this.viewCamera() as Camera & { updateMatrixWorld(): void };
    camera.updateMatrixWorld();
    const out: Array<{ id: string; inside: boolean }> = [];
    for (const id of [...p.characters.ids(), ...p.props.ids(), ...p.crowds.ids(), ...p.shots.ids()]) {
      const located = this.locate(id);
      if (!located || located.object.visible === false) continue;
      const b = _box.setFromObject(located.pick);
      let inside = true;
      for (const x of [b.min.x, b.max.x]) {
        for (const y of [b.min.y, b.max.y]) {
          for (const z of [b.min.z, b.max.z]) {
            const ndc = _v.set(x, y, z).project(camera);
            if (Math.abs(ndc.x) > 1 || Math.abs(ndc.y) > 1 || ndc.z < -1 || ndc.z > 1) inside = false;
          }
        }
      }
      out.push({ id, inside });
    }
    return out;
  }

  /**
   * three r186 给物理材质（MeshStandardMaterial）用的 DFG 查找表是**模块级单例**（16×16，`getDFGLUT()`），
   * three 自己从不释放它：不在这里释放的话，关闭编辑器后 textures 永远剩 1（DR-AC-02 实测）。
   * 释放只丢掉这个渲染器上的 GPU 副本，数据还在；下次打开时新的渲染器照常重新上传。
   * 它只挂在材质的内部 uniforms 上，从任一已渲染过的物理材质取。
   */
  private dfgLut(p: Parts): Texture | null {
    let lut: Texture | null = null;
    p.scene.traverse((o) => {
      if (lut) return;
      const m = (o as { material?: Material | Material[] }).material;
      for (const material of Array.isArray(m) ? m : m ? [m] : []) {
        const uniforms = (p.host.renderer.properties.get(material) as { uniforms?: Record<string, { value?: unknown }> }).uniforms;
        const value = uniforms?.dfgLUT?.value as Texture | undefined;
        if (value?.isTexture) lut = value;
      }
    });
    return lut;
  }

  /** 释放之后仍保留 host 的引用：上下文丢失事件是异步的（`forceContextLoss` 之后才到），调试钩子要读到它。 */
  private lastHost: RendererHost | null = null;

  private memory(): { geometries: number; textures: number } {
    const p = this.parts;
    if (p) return { geometries: p.host.renderer.info.memory.geometries, textures: p.host.renderer.info.memory.textures };
    return this.finalMemory ?? { geometries: 0, textures: 0 };
  }

  /** 调试钩子开着时的帧时间（`performance.now()`），供 `fps()`。 */
  private debugFrames: number[] | null = null;

  private installDebugHook(): void {
    const host = () => this.parts?.host ?? this.lastHost;
    this.debugFrames = [];
    const hex = (color: number) => `#${color.toString(16).padStart(6, "0")}`;
    const hook: DirectorDebugHook = {
      memory: () => this.memory(),
      contextLost: false,
      renders: 0,
      lastCapture: null,
      engine: this,
      rendererInfo: () => {
        const info = this.parts?.host.renderer.info;
        return {
          memory: this.memory(),
          render: info ? { calls: info.render.calls, triangles: info.render.triangles, points: info.render.points, lines: info.render.lines, frame: info.render.frame } : null
        };
      },
      cameraWorldPosition: (id: string) => {
        const p = this.parts;
        const camera = id === "director" ? p?.rig.camera : p?.shots.get(id)?.camera;
        if (!camera) return null;
        camera.updateMatrixWorld();
        const v = camera.getWorldPosition(new Vector3());
        return { x: round6(v.x), y: round6(v.y), z: round6(v.z) };
      },
      fps: () => {
        const now = performance.now();
        return (this.debugFrames ?? []).filter((t) => t > now - 1000).length;
      },
      helperColors: [hex(HELPER_MAGENTA), hex(HELPER_CYAN)]
    };
    Object.defineProperties(hook, {
      contextLost: { get: () => host()?.contextLost ?? false, enumerable: true },
      renders: { get: () => host()?.renders ?? 0, enumerable: true }
    });
    window.__directorDebug = hook;
  }

  /**
   * 引擎释放时要一起收掉的东西（DRT-23：姿势识别的 Worker），登记进 Disposer。没挂载时不登记，返回 false
   * （调用方在组件卸载时自己收）。
   */
  onDispose(fn: () => void): boolean {
    if (!this.parts) return false;
    this.parts.disposer.defer(fn);
    return true;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    const p = this.parts;
    this.parts = null;
    if (!p) return;
    // 兜底：场景图里还挂着、却没登记的几何与材质也收走。
    p.disposer.collect(p.scene);
    const lut = this.dfgLut(p);
    if (lut) p.disposer.texture(lut);
    p.disposer.dispose();
    // DR-AC-02 的测量点：资源都释放之后、交还上下文之前。
    this.finalMemory = { geometries: p.host.renderer.info.memory.geometries, textures: p.host.renderer.info.memory.textures };
    this.lastHost = p.host;
    p.host.dispose();
    this.events.clear();
  }
}
