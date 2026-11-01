/**
 * 时间轴与场景之间的那一层（DRT-24，dev/03 §10）：对象在某一帧显示成什么样、改动什么时候变成关键帧。纯函数。
 *
 * - 有运动轨道的对象（机位是 `camera` 通道、带焦距，其余是 `transform`），显示的是轨道在当前帧的采样，不是基础变换；
 *   有姿势轨道（`pose`）的角色同理；
 * - **自动打帧**：有轨道的对象被改了（视口拖动、数值框、焦距、换姿势、调关节），改动写成**当前帧**的关键帧（同帧覆盖）——
 *   否则「播放头在中间时拖了一下，播放时又被轨道拉回去」，用户以为改动丢了。没有轨道的对象照旧只改基础值；
 * - 一次改动 = 一次 commit = 一步撤销：基础值与关键帧一起回去（store 的 `commit` 调 `directorAutoKeyframe`）。
 *
 * ⚠️ 判断「改了没有」比的是**基础值**的前后：视口拖动把「显示的值 + 拖动量」写进基础值，数值框也以显示的值为底，
 *    所以有轨道时基础值一定会变。改到与基础值相同的情况不打帧（与没改一样，store 也不提交）。
 */
import { DIRECTOR_OBJECT_DEFAULTS } from "./directorCatalog";
import type { DirectorObject, DirectorObjectKind, DirectorScene } from "./directorScene";
import {
  directorDurationFrames,
  directorPoseAt,
  directorSampleTrack,
  directorUpsertKeyframe,
  type DirectorChannel,
  type DirectorInterpolation,
  type DirectorMotionValue,
  type DirectorPoseLookup,
  type DirectorPoseValue,
  type DirectorTimeline,
  type DirectorTimelineError,
  type DirectorTrack
} from "./directorTimeline";

export type DirectorMotionChannel = "transform" | "camera";

/**
 * 引擎内部用的字段：两个姿势之间的离地比例（插值出来的），只存在于投影给引擎的对象上，**从不写进场景**。
 * 不带它的话跳起、落地之间的过渡会在关键帧那一帧突然跳一下。
 */
export const DIRECTOR_SAMPLED_LIFT = "__sampledLift";

/** 对象的运动通道：机位是 camera（多一个焦距），其余是 transform。 */
export function directorMotionChannel(kind: DirectorObjectKind | string): DirectorMotionChannel {
  return kind === "camera" ? "camera" : "transform";
}

/** 场景的时间轴，缺省值补齐（固定 30 fps、总时长 30~450 帧、默认不循环）。不改入参。 */
export function directorTimelineOf(scene: DirectorScene | null | undefined): Required<Pick<DirectorTimeline, "fps" | "durationFrames" | "loop" | "tracks">> & DirectorTimeline {
  const raw = (scene?.timeline ?? {}) as DirectorTimeline;
  return { ...raw, fps: 30, durationFrames: directorDurationFrames(raw), loop: raw.loop === true, tracks: Array.isArray(raw.tracks) ? raw.tracks : [] };
}

export function directorTrackOf(scene: DirectorScene | null | undefined, objectId: string, channel: DirectorChannel): DirectorTrack | undefined {
  return directorTimelineOf(scene).tracks.find(track => track.objectId === objectId && track.channel === channel);
}

/** 对象当前的运动值（基础变换；机位带焦距）：打关键帧、比较改动都按它。 */
export function directorMotionValueOf(object: DirectorObject): DirectorMotionValue {
  const t = object.transform;
  const value: DirectorMotionValue = { position: { ...t.position }, rotation: { ...t.rotation } };
  if (object.kind === "camera") {
    const focal = Number((object.camera as { focalLengthMm?: unknown } | undefined)?.focalLengthMm);
    value.focalLengthMm = Number.isFinite(focal) && focal > 0 ? focal : DIRECTOR_OBJECT_DEFAULTS.camera.focalLengthMm;
  }
  return value;
}

/** 角色当前的姿势值（预设 + 覆盖）。 */
export function directorPoseValueOf(object: DirectorObject): DirectorPoseValue {
  const c = (object.character ?? {}) as { posePreset?: unknown; boneOverrides?: unknown };
  const overrides = c.boneOverrides && typeof c.boneOverrides === "object" && !Array.isArray(c.boneOverrides)
    ? { ...(c.boneOverrides as Record<string, { x: number; y: number; z: number }>) }
    : {};
  return { posePreset: typeof c.posePreset === "string" ? c.posePreset : DIRECTOR_OBJECT_DEFAULTS.character.posePreset, boneOverrides: overrides };
}

const NO_POSES: DirectorPoseLookup = () => undefined;

/**
 * 对象在第 `frame` 帧显示成什么样：有轨道的部分换成采样值（位置 / 朝向 / 焦距 / 姿势），其余原样。
 * 引擎投影、数值框显示都用它。`poses` 是姿势库（只有姿势轨道用得到）。
 */
export function directorProjectObject(
  object: DirectorObject,
  tracks: readonly DirectorTrack[],
  frame: number,
  poses: DirectorPoseLookup = NO_POSES
): DirectorObject {
  let out = object;
  const motion = tracks.find(track => track.objectId === object.id && track.channel === directorMotionChannel(object.kind));
  const sample = motion ? directorSampleTrack(motion, frame) : null;
  if (sample) {
    out = { ...out, transform: { ...out.transform, position: { ...sample.position }, rotation: { ...sample.rotation } } };
    if (object.kind === "camera" && sample.focalLengthMm !== undefined) {
      out = { ...out, camera: { ...(out.camera ?? {}), focalLengthMm: sample.focalLengthMm } };
    }
  }
  if (object.kind === "character") {
    const poseTrack = tracks.find(track => track.objectId === object.id && track.channel === "pose");
    const pose = poseTrack ? directorPoseAt(poseTrack, frame, poses) : null;
    if (pose) {
      out = {
        ...out,
        character: { ...(out.character ?? {}), posePreset: pose.posePreset ?? "", boneOverrides: pose.bones, [DIRECTOR_SAMPLED_LIFT]: pose.lift }
      };
    }
  }
  return out;
}

/**
 * 整个场景在第 `frame` 帧显示的样子（每个有轨道的对象都换成采样值）；没有轨道时原样返回同一个场景。
 * 截图的机位快照、镜头文字都按它：截下来的是这一帧的画面，记下来的也得是这一帧的机位（DRT-24 / 26）。
 */
export function directorDisplayedScene(scene: DirectorScene, frame: number, poses?: DirectorPoseLookup): DirectorScene {
  const tracks = directorTimelineOf(scene).tracks;
  if (!tracks.length) return scene;
  return { ...scene, objects: (scene.objects ?? []).map(object => directorProjectObject(object, tracks, frame, poses)) };
}

/** 场景里对象在第 `frame` 帧显示的样子（见 `directorProjectObject`）。对象不在时为 undefined。 */
export function directorDisplayedObject(scene: DirectorScene | null | undefined, objectId: string, frame: number, poses?: DirectorPoseLookup): DirectorObject | undefined {
  const object = scene?.objects?.find(o => o.id === objectId);
  return object ? directorProjectObject(object, directorTimelineOf(scene).tracks, frame, poses) : undefined;
}

function sameJson(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

function withTimeline(scene: DirectorScene, timeline: DirectorTimeline): DirectorScene {
  return { ...scene, timeline: timeline as DirectorScene["timeline"] };
}

/**
 * 自动打帧（见文件头）：`before → after` 里，有对应轨道的对象运动值 / 姿势变了，就在 `frame` 写入关键帧。
 * 超出上限时那一处不写（改动照样落在基础值上），并把原因带回去让界面说出来。
 */
export function directorAutoKeyframe(
  before: DirectorScene,
  after: DirectorScene,
  frame: number
): { scene: DirectorScene; keyed: number; error?: DirectorTimelineError } {
  const tracks = directorTimelineOf(after).tracks;
  if (!tracks.length) return { scene: after, keyed: 0 };
  const has = (id: string, channel: DirectorChannel) => tracks.some(track => track.objectId === id && track.channel === channel);
  const previous = new Map((before.objects ?? []).map(o => [o.id, o]));
  let timeline = (after.timeline ?? {}) as DirectorTimeline;
  let keyed = 0;
  let error: DirectorTimelineError | undefined;
  const key = (id: string, channel: DirectorChannel, value: DirectorMotionValue | DirectorPoseValue) => {
    const result = directorUpsertKeyframe(timeline, id, channel, frame, value);
    if ("error" in result) error ??= result;
    else {
      timeline = result.timeline;
      keyed += 1;
    }
  };
  for (const object of after.objects ?? []) {
    const old = previous.get(object.id);
    if (!old) continue;
    const channel = directorMotionChannel(object.kind);
    if (has(object.id, channel)) {
      const value = directorMotionValueOf(object);
      if (!sameJson(value, directorMotionValueOf(old))) key(object.id, channel, value);
    }
    if (object.kind === "character" && has(object.id, "pose")) {
      const value = directorPoseValueOf(object);
      if (!sameJson(value, directorPoseValueOf(old))) key(object.id, "pose", value);
    }
  }
  return { scene: keyed ? withTimeline(after, timeline) : after, keyed, ...(error ? { error } : {}) };
}

/**
 * 时间轴「在当前帧打关键帧」：记录对象在这一帧**显示**的状态（已有轨道时就是采样值，否则是基础值）。
 * 机位记位置 / 朝向 / 焦距（camera 通道），其余对象记位置 / 朝向（transform 通道），角色姿势另记 pose 通道。
 */
export function directorKeyObjectAt(
  scene: DirectorScene,
  objectId: string,
  channel: DirectorChannel,
  frame: number,
  poses?: DirectorPoseLookup,
  interpolation?: DirectorInterpolation
): { scene: DirectorScene } | DirectorTimelineError {
  const shown = directorDisplayedObject(scene, objectId, frame, poses);
  if (!shown) return { error: "missing" };
  let value: DirectorMotionValue | DirectorPoseValue;
  if (channel === "pose") {
    if (shown.kind !== "character") return { error: "missing" };
    const c = shown.character as Record<string, unknown>;
    value = directorPoseValueOf({ ...shown, character: { posePreset: c.posePreset, boneOverrides: c.boneOverrides } });
  } else {
    if (channel !== directorMotionChannel(shown.kind)) return { error: "missing" };
    value = directorMotionValueOf(shown);
  }
  const result = directorUpsertKeyframe(directorTimelineOf(scene), objectId, channel, frame, value, interpolation);
  return "error" in result ? result : { scene: withTimeline(scene, result.timeline) };
}

/** 对时间轴做一次编辑（挪关键帧、删关键帧、改插值……）：成功返回新场景，失败原样带回原因。 */
export function directorEditTimeline(
  scene: DirectorScene,
  edit: (timeline: DirectorTimeline) => { timeline: DirectorTimeline } | DirectorTimelineError
): { scene: DirectorScene } | DirectorTimelineError {
  const result = edit(directorTimelineOf(scene));
  if ("error" in result) return result;
  return { scene: withTimeline(scene, result.timeline) };
}

/** 改总时长 / 循环开关（缩短前要先过 `directorCanShrink`）。值没变时返回同一个场景。 */
export function directorSetTimelineSettings(scene: DirectorScene, patch: { durationFrames?: number; loop?: boolean }): DirectorScene {
  const current = directorTimelineOf(scene);
  const next = { ...current, ...patch };
  if (next.durationFrames === current.durationFrames && next.loop === current.loop) return scene;
  return withTimeline(scene, next);
}

// ---------------------------------------------------------------- 时间轴的行（DRT-24）

/** 时间轴上选中的一个关键帧（对象 + 通道 + 帧号）。 */
export type DirectorKeyRef = { objectId: string; channel: DirectorChannel; frame: number };

export type DirectorTimelineRow = {
  objectId: string;
  objectName: string;
  kind: DirectorObjectKind;
  channel: DirectorChannel;
  /** 行名：「机位 1」「角色 1 · 位置」「角色 1 · 姿势」。 */
  label: string;
  keyframes: DirectorTrack["keyframes"];
};

const KIND_ORDER: Record<string, number> = { camera: 0, character: 1, crowd: 2, prop: 3 };

/**
 * 时间轴上显示哪些行：有关键帧的对象、以及当前选中的对象（好给它打第一个关键帧）。
 * 机位一行（运动 + 焦距），角色两行（位置、姿势），群众与道具一行；按机位 → 角色 → 群众 → 道具、再按场景里的顺序。
 */
export function directorTimelineRows(scene: DirectorScene | null | undefined, selectedId: string | null): DirectorTimelineRow[] {
  const tracks = directorTimelineOf(scene).tracks;
  const objects = (scene?.objects ?? []).filter(o => o.id === selectedId || tracks.some(t => t.objectId === o.id));
  const ordered = objects
    .map((object, index) => ({ object, index }))
    .sort((a, b) => (KIND_ORDER[a.object.kind] ?? 9) - (KIND_ORDER[b.object.kind] ?? 9) || a.index - b.index)
    .map(item => item.object);
  const rows: DirectorTimelineRow[] = [];
  for (const object of ordered) {
    const name = object.name || object.id;
    const channels: DirectorChannel[] = object.kind === "character" ? ["transform", "pose"] : [directorMotionChannel(object.kind)];
    for (const channel of channels) {
      const track = tracks.find(t => t.objectId === object.id && t.channel === channel);
      if (!track && object.id !== selectedId) continue;
      const label = object.kind === "character" ? `${name} · ${channel === "pose" ? "姿势" : "位置"}` : name;
      rows.push({ objectId: object.id, objectName: name, kind: object.kind, channel, label, keyframes: track?.keyframes ?? [] });
    }
  }
  return rows;
}
