/**
 * 3D 导演台的场景规则（DRT-11，dev/02 §2.4，dev/01 §4.1）：校验、迁移、对象增删改、命名与上限。
 * 纯函数，不 import three、不碰网络；引擎（DRT-12）、store（DRT-14）、界面（DRT-15）、截图（DRT-17）都只调这里。
 *
 * ⚠️ 校验与 Go 的 `director/domain.Validate` 是**同一套规则、同一个遍历顺序**，对同一份坏数据报**同一个 path**
 * （共享 fixture `api/fixtures/director/scenes/`）。前后端说法不一致的话，同一份坏数据保存前报一处、
 * 保存后又报另一处，用户无从下手。改规则时两边一起改，fixture 会把只改一边的那次拦下来。
 * - 只拦结构性损坏与上限，**不顺手修复、不四舍五入**：改数值会让「用户输入的就是保存的」不成立；
 * - 未知字段不报错（GO-35，配置可以比代码新）；
 * - 姿势码未知时放行（前端姿势资源可能比后端新），素模码必须认识；
 * - 前端多查一条 Go 查不到的：数值必须有限。JSON 里写不出 NaN / Infinity，但内存里的场景对象可以有，
 *   `JSON.stringify` 会把它们悄悄变成 null —— 存进去就是一个缺字段的场景。
 *
 * ⚠️ 对象操作一律不可变（返回新场景，不改入参）：store 的撤销栈按快照工作（DRD-36），
 * 原地改对象会让上一步快照跟着变，撤销失效且不报错。
 */
import {
  DIRECTOR_ASPECTS,
  DIRECTOR_BODIES,
  DIRECTOR_LIMITS,
  DIRECTOR_MODELS,
  DIRECTOR_OBJECT_DEFAULTS,
  DIRECTOR_PROP_SHAPES,
  DIRECTOR_SCHEMA_VERSION
} from "./directorCatalog";

export type DirectorVec3 = { x: number; y: number; z: number };
export type DirectorObjectKind = "character" | "crowd" | "prop" | "camera";
export type DirectorSource = "user" | "ai";

/** 场景里的一个对象。子结构与未知字段原样保留，所以都是宽松类型。 */
export type DirectorObject = {
  id: string;
  name: string;
  kind: DirectorObjectKind;
  transform: { position: DirectorVec3; rotation: DirectorVec3; scale: DirectorVec3 } & Record<string, unknown>;
  visible?: boolean;
  locked?: boolean;
  source?: DirectorSource;
  [key: string]: unknown;
};

/** 场景正文。只列前端会读写的字段，其余（含未来字段）原样保留。 */
export type DirectorScene = {
  schemaVersion: number;
  objects?: DirectorObject[];
  outputCameraId?: string | null;
  timeline?: { tracks?: Array<{ objectId: string } & Record<string, unknown>> } & Record<string, unknown>;
  [key: string]: unknown;
};

export type DirectorValidation = { ok: true } | { ok: false; path: string; message: string; limit?: number };

/** 上限错误（DR-F-13）：带上限值，界面据此说清「最多 N 个」。 */
export type DirectorLimitError = { error: "limit"; what: "objects" | "crowd" | "sceneBytes"; limit: number };

// ---------------------------------------------------------------- 校验

/** 时间轴口径（DR-PARAM-10）：固定 30fps，总时长 30~450 帧。与 Go 的常量一致。 */
const TIMELINE_FPS = 30;
const MIN_DURATION_FRAMES = 30;
const MAX_DURATION_FRAMES = 450;
const MAX_ROTATION = 36000;
const MAX_BONE_ROTATION = 360;
const MAX_POSE_CODE_CHARS = 64;
const MAX_INT32 = 2147483647;
const MAX_INT64 = 9223372036854775807n;

const MODEL_SET = new Set<string>(DIRECTOR_MODELS.map((m) => m.code));
const BODY_SET = new Set<string>(DIRECTOR_BODIES.map((b) => b.code));
const PROP_SHAPE_SET = new Set<string>(DIRECTOR_PROP_SHAPES);
const ASPECT_SET = new Set<string>(DIRECTOR_ASPECTS.map((a) => a.code));
const COLOR_RE = /^#[0-9a-fA-F]{6}$/;

type Json = Record<string, unknown>;

function isObject(v: unknown): v is Json {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

const has = (m: Json, key: string) => Object.prototype.hasOwnProperty.call(m, key);
const join = (base: string, key: string) => (base === "" ? key : `${base}.${key}`);
const index = (base: string, i: number) => `${base}[${i}]`;
const chars = (s: string) => Array.from(s).length;

/** 按码点比较（= Go 按 UTF-8 字节排序的结果），boneOverrides 的遍历顺序与 Go 的 `sortedKeys` 一致。 */
function compareCodePoints(a: string, b: string): number {
  const ca = Array.from(a);
  const cb = Array.from(b);
  for (let i = 0; i < Math.min(ca.length, cb.length); i++) {
    const d = (ca[i]?.codePointAt(0) ?? 0) - (cb[i]?.codePointAt(0) ?? 0);
    if (d !== 0) return d;
  }
  return ca.length - cb.length;
}

/** 按固定顺序遍历，遇到第一处错误就停：同一份坏数据每次报同一个 path（与 Go 的 checker 一一对应）。 */
class Checker {
  err: { path: string; message: string; limit?: number } | null = null;

  fail(path: string, message: string, limit?: number) {
    if (!this.err) this.err = limit === undefined ? { path, message } : { path, message, limit };
  }

  get ok() {
    return this.err === null;
  }

  root(m: Json) {
    if (!has(m, "schemaVersion")) return this.fail("schemaVersion", "缺少结构版本");
    const sv = m.schemaVersion;
    if (typeof sv !== "number" || !Number.isInteger(sv)) return this.fail("schemaVersion", "结构版本必须是整数");
    if (sv !== DIRECTOR_SCHEMA_VERSION) {
      return this.fail("schemaVersion", `不支持的结构版本 ${sv}（当前为 ${DIRECTOR_SCHEMA_VERSION}）`);
    }

    const stage = this.optObject(m, "stage", "");
    if (stage) {
      this.number(stage, "scale", "stage", 0.1, 10, false);
      this.optVec3(stage, "offset", "stage", DIRECTOR_LIMITS.maxCoord);
      this.optVec3(stage, "rotation", "stage", MAX_ROTATION);
    }
    const view = this.optObject(m, "view", "");
    if (view) {
      this.optBool(view, "showLabels", "view");
      const grid = this.optObject(view, "gridSnap", "view");
      if (grid) {
        this.optBool(grid, "enabled", "view.gridSnap");
        this.optOneOf(grid, "step", "view.gridSnap", [0.1, 0.25, 0.5, 1]);
      }
      const angle = this.optObject(view, "angleSnap", "view");
      if (angle) {
        this.optBool(angle, "enabled", "view.angleSnap");
        this.optOneOf(angle, "step", "view.angleSnap", [5, 15, 45]);
      }
      this.optBool(view, "groundSnap", "view");
    }
    const background = this.optObject(m, "background", "");
    if (background) this.background(background);
    const splat = this.optObject(m, "splat", "");
    if (splat) {
      this.mediaId(splat, "mediaId", "splat", true);
      this.optVec3(splat, "position", "splat", DIRECTOR_LIMITS.maxCoord);
      this.optVec3(splat, "rotation", "splat", MAX_ROTATION);
      this.number(splat, "scale", "splat", 0.01, 100, false);
      this.optBool(splat, "visible", "splat");
    }
    const ground = this.optObject(m, "ground", "");
    if (ground) {
      this.optBool(ground, "visible", "ground");
      this.number(ground, "opacity", "ground", 0, 1, false);
      this.number(ground, "height", "ground", -100, 100, false);
    }
    const lights = this.optObject(m, "lights", "");
    if (lights) {
      for (const key of ["key", "fill"]) {
        const light = this.optObject(lights, key, "lights");
        if (light) this.light(light, join("lights", key));
      }
    }
    if (!this.ok) return;

    const kinds = this.objects(m);
    if (!this.ok) return;
    this.outputCamera(m, kinds);
    if (!this.ok) return;
    const timeline = this.optObject(m, "timeline", "");
    if (timeline) this.timeline(timeline, kinds);
  }

  background(b: Json) {
    const mode = typeof b.mode === "string" ? b.mode : "";
    if (has(b, "mode")) this.enum(b, "mode", "background", ["color", "panorama"]);
    this.optColor(b, "color", "background");
    const panorama = this.optObject(b, "panorama", "background");
    if (mode === "panorama" && !panorama && this.ok) {
      return this.fail("background.panorama", "背景为全景图时必须指定全景来源");
    }
    if (panorama) {
      this.str(panorama, "sourceNodeId", "background.panorama", DIRECTOR_LIMITS.maxIdChars, true);
      this.mediaId(panorama, "mediaId", "background.panorama", true);
      this.number(panorama, "yaw", "background.panorama", 0, 360, false);
      this.number(panorama, "radius", "background.panorama", 5, 200, false);
    }
  }

  light(l: Json, path: string) {
    this.optBool(l, "visible", path);
    this.number(l, "intensity", path, 0, 10, false);
    this.optColor(l, "color", path);
    this.number(l, "azimuth", path, 0, 360, false);
    this.number(l, "elevation", path, -90, 90, false);
  }

  /** 校验对象数组，返回 ID → 种类（给机位与时间轴校验用）。 */
  objects(m: Json): Map<string, string> {
    const kinds = new Map<string, string>();
    if (!has(m, "objects")) return kinds;
    const list = m.objects;
    if (!Array.isArray(list)) {
      this.fail("objects", "必须是数组");
      return kinds;
    }
    if (list.length > DIRECTOR_LIMITS.maxObjects) {
      this.fail("objects", `对象数 ${list.length} 超过上限 ${DIRECTOR_LIMITS.maxObjects}`, DIRECTOR_LIMITS.maxObjects);
      return kinds;
    }
    for (const [i, item] of list.entries()) {
      const path = index("objects", i);
      if (!isObject(item)) {
        this.fail(path, "必须是对象");
        return kinds;
      }
      const id = this.str(item, "id", path, DIRECTOR_LIMITS.maxIdChars, true);
      if (!this.ok) return kinds;
      if (kinds.has(id)) {
        this.fail(join(path, "id"), `对象 ID 重复：${id}`);
        return kinds;
      }
      this.str(item, "name", path, DIRECTOR_LIMITS.maxNameChars, true);
      const kind = this.enum(item, "kind", path, ["character", "crowd", "prop", "camera"]);
      if (!this.ok) return kinds;
      kinds.set(id, kind);
      this.transform(item, path);
      this.optBool(item, "visible", path);
      this.optBool(item, "locked", path);
      if (has(item, "source")) this.enum(item, "source", path, ["user", "ai"]);
      if (kind === "character") this.character(item, path);
      else if (kind === "crowd") this.crowd(item, path);
      else if (kind === "prop") this.prop(item, path);
      else if (kind === "camera") this.camera(item, path);
      if (!this.ok) return kinds;
    }
    return kinds;
  }

  transform(o: Json, path: string) {
    const t = this.reqObject(o, "transform", path);
    if (!t) return;
    const tp = join(path, "transform");
    this.reqVec3(t, "position", tp, DIRECTOR_LIMITS.maxCoord);
    this.reqVec3(t, "rotation", tp, MAX_ROTATION);
    this.reqScale3(t, "scale", tp);
  }

  character(o: Json, path: string) {
    const ch = this.reqObject(o, "character", path);
    if (!ch) return;
    const p = join(path, "character");
    this.model(ch, p);
    this.number(ch, "height", p, 0.8, 2.3, true);
    this.enumSet(ch, "body", p, BODY_SET, true);
    this.str(ch, "posePreset", p, MAX_POSE_CODE_CHARS, false);
    this.boneOverrides(ch, p);
    const footLock = this.optObject(ch, "footLock", p);
    if (footLock) {
      this.optBool(footLock, "left", join(p, "footLock"));
      this.optBool(footLock, "right", join(p, "footLock"));
    }
  }

  crowd(o: Json, path: string) {
    const cr = this.reqObject(o, "crowd", path);
    if (!cr) return;
    const p = join(path, "crowd");
    this.model(cr, p);
    const rows = this.integer(cr, "rows", p, 1, DIRECTOR_LIMITS.maxCrowdRows, true);
    const cols = this.integer(cr, "cols", p, 1, DIRECTOR_LIMITS.maxCrowdCols, true);
    if (this.ok && rows * cols > DIRECTOR_LIMITS.maxCrowdSize) {
      return this.fail(p, `单阵人数 ${rows * cols} 超过上限 ${DIRECTOR_LIMITS.maxCrowdSize}`, DIRECTOR_LIMITS.maxCrowdSize);
    }
    this.number(cr, "spacing", p, 0.5, 5, true);
    this.str(cr, "posePreset", p, MAX_POSE_CODE_CHARS, false);
    this.optBool(cr, "jitter", p);
    this.integer(cr, "seed", p, 0, MAX_INT32, false);
  }

  prop(o: Json, path: string) {
    const pr = this.reqObject(o, "prop", path);
    if (!pr) return;
    const p = join(path, "prop");
    this.enumSet(pr, "shape", p, PROP_SHAPE_SET, true);
    this.reqScale3(pr, "size", p);
    this.optColor(pr, "color", p);
  }

  camera(o: Json, path: string) {
    const ca = this.reqObject(o, "camera", path);
    if (!ca) return;
    const p = join(path, "camera");
    this.number(ca, "focalLengthMm", p, 12, 200, true);
    this.number(ca, "sensorWidthMm", p, 10, 70, false);
    if (this.ok) {
      const v = ca.aspect;
      if (!has(ca, "aspect")) this.fail(join(p, "aspect"), "缺少画幅");
      else if (typeof v !== "string") this.fail(join(p, "aspect"), "画幅必须是字符串");
      else if (!ASPECT_SET.has(v)) this.fail(join(p, "aspect"), `不支持的画幅 ${JSON.stringify(v)}`);
    }
    const near = this.number(ca, "near", p, 0.01, 10, false);
    const far = this.number(ca, "far", p, 10, 10000, false);
    if (this.ok && near > 0 && far > 0 && near >= far) this.fail(join(p, "near"), "近裁剪面必须小于远裁剪面");
    this.number(ca, "roll", p, -180, 180, false);
  }

  model(m: Json, path: string) {
    if (!this.ok) return;
    const v = m.model;
    if (!has(m, "model")) this.fail(join(path, "model"), "缺少素模");
    else if (typeof v !== "string" || !MODEL_SET.has(v)) this.fail(join(path, "model"), `不认识的素模 ${String(v)}`);
  }

  boneOverrides(m: Json, path: string) {
    if (!this.ok) return;
    const v = m.boneOverrides;
    if (!has(m, "boneOverrides") || v === null) return;
    const p = join(path, "boneOverrides");
    if (!isObject(v)) return this.fail(p, "必须是对象（骨骼名 → 旋转）");
    const names = Object.keys(v).sort(compareCodePoints);
    if (names.length > DIRECTOR_LIMITS.maxBoneOverrides) {
      return this.fail(p, `骨骼覆盖 ${names.length} 项超过上限 ${DIRECTOR_LIMITS.maxBoneOverrides}`, DIRECTOR_LIMITS.maxBoneOverrides);
    }
    for (const name of names) {
      if (chars(name) > MAX_POSE_CODE_CHARS) return this.fail(p, `骨骼名过长：${name}`);
      this.vec3Value(v[name], join(p, name), MAX_BONE_ROTATION);
      if (!this.ok) return;
    }
  }

  outputCamera(m: Json, kinds: Map<string, string>) {
    if (!has(m, "outputCameraId") || m.outputCameraId === null) return;
    const id = m.outputCameraId;
    if (typeof id !== "string") return this.fail("outputCameraId", "必须是字符串或 null");
    if (kinds.get(id) !== "camera") this.fail("outputCameraId", `输出机位 ${JSON.stringify(id)} 不存在`);
  }

  timeline(t: Json, kinds: Map<string, string>) {
    if (has(t, "fps") && t.fps !== TIMELINE_FPS) return this.fail("timeline.fps", `帧率固定为 ${TIMELINE_FPS}`);
    let duration = 150;
    if (has(t, "durationFrames")) {
      duration = this.integer(t, "durationFrames", "timeline", MIN_DURATION_FRAMES, MAX_DURATION_FRAMES, true);
    }
    this.optBool(t, "loop", "timeline");
    if (!this.ok || !has(t, "tracks")) return;
    const tracks = t.tracks;
    if (!Array.isArray(tracks)) return this.fail("timeline.tracks", "必须是数组");
    let total = 0;
    for (const [i, item] of tracks.entries()) {
      const path = index("timeline.tracks", i);
      if (!isObject(item)) return this.fail(path, "必须是对象");
      const objectId = this.str(item, "objectId", path, DIRECTOR_LIMITS.maxIdChars, true);
      if (!this.ok) return;
      const kind = kinds.get(objectId);
      if (kind === undefined) return this.fail(join(path, "objectId"), `对象 ${JSON.stringify(objectId)} 不存在`);
      const channel = this.enum(item, "channel", path, ["transform", "pose", "camera"]);
      if (!this.ok) return;
      // 通道与对象种类要对得上：机位用 camera 通道（含焦距），姿势只对角色有意义。
      if (channel === "camera" && kind !== "camera") return this.fail(join(path, "channel"), "camera 通道只能挂在机位上");
      if (channel === "pose" && kind !== "character") return this.fail(join(path, "channel"), "pose 通道只能挂在角色上");
      if (channel === "transform" && kind === "camera") return this.fail(join(path, "channel"), "机位的关键帧用 camera 通道");
      const keyframes = item.keyframes;
      if (!has(item, "keyframes") || !Array.isArray(keyframes)) return this.fail(join(path, "keyframes"), "必须是数组");
      if (keyframes.length > DIRECTOR_LIMITS.maxKeyframesPerTrack) {
        return this.fail(
          join(path, "keyframes"),
          `单条轨道关键帧 ${keyframes.length} 个超过上限 ${DIRECTOR_LIMITS.maxKeyframesPerTrack}`,
          DIRECTOR_LIMITS.maxKeyframesPerTrack
        );
      }
      total += keyframes.length;
      if (total > DIRECTOR_LIMITS.maxKeyframes) {
        return this.fail("timeline.tracks", `全场景关键帧超过上限 ${DIRECTOR_LIMITS.maxKeyframes}`, DIRECTOR_LIMITS.maxKeyframes);
      }
      const seen = new Set<number>();
      for (const [j, kitem] of keyframes.entries()) {
        const kp = index(join(path, "keyframes"), j);
        if (!isObject(kitem)) return this.fail(kp, "必须是对象");
        const frame = this.integer(kitem, "frame", kp, 0, MAX_INT32, true);
        if (!this.ok) return;
        if (frame > duration) return this.fail(join(kp, "frame"), `帧号 ${frame} 超出时长 ${duration} 帧`);
        if (seen.has(frame)) return this.fail(join(kp, "frame"), `同一轨道里帧号 ${frame} 重复`);
        seen.add(frame);
        if (has(kitem, "interpolation")) this.enum(kitem, "interpolation", kp, ["linear", "smooth", "hold"]);
        this.keyframeValue(kitem, kp, channel);
        if (!this.ok) return;
      }
    }
  }

  keyframeValue(kf: Json, path: string, channel: string) {
    const val = this.reqObject(kf, "value", path);
    if (!val) return;
    const vp = join(path, "value");
    if (channel === "transform" || channel === "camera") {
      this.reqVec3(val, "position", vp, DIRECTOR_LIMITS.maxCoord);
      this.reqVec3(val, "rotation", vp, MAX_ROTATION);
      if (channel === "camera") this.number(val, "focalLengthMm", vp, 12, 200, true);
    } else if (channel === "pose") {
      this.str(val, "posePreset", vp, MAX_POSE_CODE_CHARS, false);
      this.boneOverrides(val, vp);
    }
  }

  // ---------------------------------------------------------------- 基本类型

  optObject(m: Json, key: string, base: string): Json | null {
    if (!this.ok || !has(m, key)) return null;
    const v = m[key];
    if (!isObject(v)) {
      this.fail(join(base, key), "必须是对象");
      return null;
    }
    return v;
  }

  reqObject(m: Json, key: string, base: string): Json | null {
    if (!this.ok) return null;
    if (!has(m, key)) {
      this.fail(join(base, key), "缺少必填字段");
      return null;
    }
    return this.optObject(m, key, base);
  }

  /** 数值范围（闭区间）；缺省且非必填时放行并返回 0。 */
  number(m: Json, key: string, base: string, lo: number, hi: number, required: boolean): number {
    if (!this.ok) return 0;
    if (!has(m, key)) {
      if (required) this.fail(join(base, key), "缺少必填字段");
      return 0;
    }
    const v = m[key];
    if (typeof v !== "number") {
      this.fail(join(base, key), "必须是数字");
      return 0;
    }
    if (!Number.isFinite(v)) {
      this.fail(join(base, key), "必须是有限的数（不能是 NaN 或无穷大）");
      return 0;
    }
    if (v < lo || v > hi) {
      this.fail(join(base, key), `取值 ${v} 超出范围 ${lo}~${hi}`);
      return 0;
    }
    return v;
  }

  integer(m: Json, key: string, base: string, lo: number, hi: number, required: boolean): number {
    if (!this.ok) return 0;
    if (!has(m, key)) {
      if (required) this.fail(join(base, key), "缺少必填字段");
      return 0;
    }
    const v = m[key];
    if (typeof v !== "number" || !Number.isInteger(v)) {
      this.fail(join(base, key), "必须是整数");
      return 0;
    }
    if (v < lo || v > hi) {
      this.fail(join(base, key), `取值 ${v} 超出范围 ${lo}~${hi}`);
      return 0;
    }
    return v;
  }

  optOneOf(m: Json, key: string, base: string, allowed: number[]) {
    if (!this.ok || !has(m, key)) return;
    const v = m[key];
    if (typeof v !== "number") return this.fail(join(base, key), "必须是数字");
    if (!allowed.includes(v)) this.fail(join(base, key), `取值 ${v} 不在可选档位内`);
  }

  optBool(m: Json, key: string, base: string) {
    if (!this.ok || !has(m, key)) return;
    if (typeof m[key] !== "boolean") this.fail(join(base, key), "必须是布尔值");
  }

  /** 字符串长度按字符数（码点）算，与 Go 的 rune 计数一致；必填时必须存在且非空。 */
  str(m: Json, key: string, base: string, maxChars: number, required: boolean): string {
    if (!this.ok) return "";
    if (!has(m, key)) {
      if (required) this.fail(join(base, key), "缺少必填字段");
      return "";
    }
    const v = m[key];
    if (typeof v !== "string") {
      this.fail(join(base, key), "必须是字符串");
      return "";
    }
    if (required && v === "") {
      this.fail(join(base, key), "不能为空");
      return "";
    }
    if (chars(v) > maxChars) {
      this.fail(join(base, key), `长度超过 ${maxChars} 个字符`);
      return "";
    }
    return v;
  }

  enum(m: Json, key: string, base: string, allowed: string[]): string {
    return this.enumSet(m, key, base, new Set(allowed), true);
  }

  enumSet(m: Json, key: string, base: string, allowed: Set<string>, required: boolean): string {
    if (!this.ok) return "";
    if (!has(m, key)) {
      if (required) this.fail(join(base, key), "缺少必填字段");
      return "";
    }
    const v = m[key];
    if (typeof v !== "string" || !allowed.has(v)) {
      this.fail(join(base, key), `不支持的取值 ${JSON.stringify(v)}`);
      return "";
    }
    return v;
  }

  optColor(m: Json, key: string, base: string) {
    if (!this.ok || !has(m, key)) return;
    const v = m[key];
    if (typeof v !== "string" || !COLOR_RE.test(v)) this.fail(join(base, key), "颜色必须是 #RRGGBB");
  }

  /** 媒体 ID：对外一律字符串（防 JS 精度丢失），内容是 int64 范围内的正整数（同 Go 的 strconv.ParseInt）。 */
  mediaId(m: Json, key: string, base: string, required: boolean) {
    if (!this.ok) return;
    if (!has(m, key)) {
      if (required) this.fail(join(base, key), "缺少必填字段");
      return;
    }
    const v = m[key];
    if (typeof v !== "string") return this.fail(join(base, key), "媒体 ID 必须是字符串");
    const valid = /^[+-]?\d+$/.test(v) && BigInt(v) > 0n && BigInt(v) <= MAX_INT64;
    if (!valid) this.fail(join(base, key), `媒体 ID 不合法：${JSON.stringify(v)}`);
  }

  optVec3(m: Json, key: string, base: string, bound: number) {
    if (!this.ok || !has(m, key)) return;
    this.vec3Value(m[key], join(base, key), bound);
  }

  reqVec3(m: Json, key: string, base: string, bound: number) {
    if (!this.ok) return;
    if (!has(m, key)) return this.fail(join(base, key), "缺少必填字段");
    this.vec3Value(m[key], join(base, key), bound);
  }

  vec3Value(v: unknown, path: string, bound: number) {
    if (!isObject(v)) return this.fail(path, "必须是 {x, y, z}");
    for (const axis of ["x", "y", "z"]) {
      this.number(v, axis, path, -bound, bound, true);
      if (!this.ok) return;
    }
  }

  /** 缩放 / 尺寸：三个分量都在 0.01~100。 */
  reqScale3(m: Json, key: string, base: string) {
    if (!this.ok) return;
    if (!has(m, key)) return this.fail(join(base, key), "缺少必填字段");
    const v = m[key];
    const p = join(base, key);
    if (!isObject(v)) return this.fail(p, "必须是 {x, y, z}");
    for (const axis of ["x", "y", "z"]) {
      this.number(v, axis, p, 0.01, 100, true);
      if (!this.ok) return;
    }
  }
}

/** 场景正文序列化后的 UTF-8 字节数（与服务端按请求原始字节比上限是同一个量）。 */
export function directorSceneBytes(scene: unknown): number {
  return new TextEncoder().encode(JSON.stringify(scene)).length;
}

/**
 * 保存前校验（DR-AC-50）。规则、遍历顺序与报出的 path 与 Go 的 `Validate` 一致；
 * 正文超过 512 KiB 时 path 为 `$` 并带上限（服务端回 41301）。
 */
export function directorValidateScene(scene: unknown): DirectorValidation {
  let bytes: number;
  try {
    bytes = directorSceneBytes(scene);
  } catch {
    return { ok: false, path: "$", message: "场景无法序列化成 JSON" };
  }
  if (bytes > DIRECTOR_LIMITS.maxSceneBytes) {
    return { ok: false, path: "$", message: `场景正文 ${bytes} 字节，超过 512 KiB 上限`, limit: DIRECTOR_LIMITS.maxSceneBytes };
  }
  if (!isObject(scene)) return { ok: false, path: "$", message: "场景正文必须是 JSON 对象" };
  const checker = new Checker();
  checker.root(scene);
  return checker.err ? { ok: false, ...checker.err } : { ok: true };
}

/**
 * 迁移到当前结构版本。v1 是第一版，**原样返回同一个对象**（不重建：未知字段与数字写法都不能变）。
 * 比前端认识的版本新时返回 `newer`，界面提示「场景是新版本创建的，请刷新页面」——照着旧规则编辑新结构，
 * 存回去就会丢掉新版本的字段。版本缺失或不是整数时原样返回，交给校验报具体 path。
 */
export function directorMigrateScene(scene: DirectorScene): { scene: DirectorScene } | { error: "newer" } {
  const v = scene.schemaVersion;
  if (typeof v === "number" && v > DIRECTOR_SCHEMA_VERSION) return { error: "newer" };
  return { scene };
}

// ---------------------------------------------------------------- 对象操作（全部不可变）

const NAME_PREFIX: Record<DirectorObjectKind, string> = { character: "角色", camera: "机位", prop: "道具", crowd: "群众" };
const ID_ALPHABET = "abcdefghijklmnopqrstuvwxyz0123456789";

/** 取随机字节的函数（测试里注入固定序列）。 */
export type DirectorRandomBytes = (n: number) => Uint8Array;

const cryptoBytes: DirectorRandomBytes = (n) => globalThis.crypto.getRandomValues(new Uint8Array(n));

/** 新对象 ID：`obj_` + 12 位小写字母数字（`crypto.getRandomValues`）。 */
export function directorNewObjectId(random: DirectorRandomBytes = cryptoBytes): string {
  return `obj_${Array.from(random(12), (b) => ID_ALPHABET[b % ID_ALPHABET.length]).join("")}`;
}

function truncateName(name: string): string {
  return Array.from(name).slice(0, DIRECTOR_LIMITS.maxNameChars).join("");
}

function escapeRe(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** `base N` 形式的名字里最大的 N（没有时为 0）。 */
function maxNumber(objects: readonly DirectorObject[], base: string): number {
  const re = new RegExp(`^${escapeRe(base)} (\\d+)$`);
  return objects.reduce((max, o) => {
    const m = re.exec(o.name);
    return m ? Math.max(max, Number(m[1])) : max;
  }, 0);
}

/** 默认名称「角色 N / 机位 N / 道具 N / 群众 N」：取同类当前最大号 + 1（DR-F-14）。 */
export function directorNextName(scene: DirectorScene, kind: DirectorObjectKind): string {
  return `${NAME_PREFIX[kind]} ${maxNumber(scene.objects ?? [], NAME_PREFIX[kind]) + 1}`;
}

function cloneJson<T>(value: T): T {
  return value === undefined ? value : (JSON.parse(JSON.stringify(value)) as T);
}

function vec(x: number, y: number, z: number): DirectorVec3 {
  return { x, y, z };
}

function newSubStructure(kind: DirectorObjectKind): Record<string, unknown> {
  const d = DIRECTOR_OBJECT_DEFAULTS;
  if (kind === "character") {
    const model = DIRECTOR_MODELS.find((m) => m.code === d.character.model);
    return { character: { ...cloneJson(d.character), height: model?.heightM ?? 1.8 } };
  }
  if (kind === "camera") return { camera: cloneJson(d.camera) };
  if (kind === "prop") return { prop: cloneJson(d.prop) };
  return { crowd: cloneJson(d.crowd) };
}

function limitError(scene: DirectorScene): DirectorLimitError | null {
  return (scene.objects?.length ?? 0) >= DIRECTOR_LIMITS.maxObjects
    ? { error: "limit", what: "objects", limit: DIRECTOR_LIMITS.maxObjects }
    : null;
}

/**
 * 加一个对象：按 `directorNextName` 命名，位置取 `at`（导演视角视线与地面的交点，由引擎算好传入）或原点。
 * 第 201 个返回上限错误（DR-F-13）。场景里还没有输出机位时，新加的机位成为输出机位（与删除时自动改选同一个道理：
 * 有机位却没有输出机位的话，节点没有截图可出，而用户看不出为什么）。
 */
export function directorAddObject(
  scene: DirectorScene,
  kind: DirectorObjectKind,
  at?: DirectorVec3,
  random?: DirectorRandomBytes
): { scene: DirectorScene; object: DirectorObject } | DirectorLimitError {
  const limit = limitError(scene);
  if (limit) return limit;
  const object: DirectorObject = {
    id: directorNewObjectId(random),
    name: directorNextName(scene, kind),
    kind,
    transform: { position: at ? vec(at.x, at.y, at.z) : vec(0, 0, 0), rotation: vec(0, 0, 0), scale: vec(1, 1, 1) },
    visible: true,
    locked: false,
    source: "user",
    ...newSubStructure(kind)
  };
  const next: DirectorScene = { ...scene, objects: [...(scene.objects ?? []), object] };
  if (kind === "camera" && (scene.outputCameraId === undefined || scene.outputCameraId === null)) {
    next.outputCameraId = object.id;
  }
  return { scene: next, object };
}

/** 复制名称：`林澈` → `林澈 2`，`角色 3` → 同类最大号 + 1（与 `directorNextName` 一致）。不超过 20 字。 */
function duplicateName(objects: readonly DirectorObject[], name: string): string {
  const base = name.replace(/ \d+$/, "");
  return truncateName(`${base} ${Math.max(maxNumber(objects, base), 1) + 1}`);
}

/**
 * 复制对象（DR-F-10）：新 id、名称自增、位置 x + 0.5 m，子结构与未知字段整份深拷贝。
 * 副本是用户这一次操作的产物：`source` 记 user、不锁定（不然它会被下一次 AI 搭景当成可替换的对象换掉，
 * 或者一出来就拖不动）。
 */
export function directorDuplicateObject(
  scene: DirectorScene,
  id: string,
  random?: DirectorRandomBytes
): { scene: DirectorScene; object: DirectorObject } | DirectorLimitError | { error: "not_found" } {
  const objects = scene.objects ?? [];
  const original = objects.find((o) => o.id === id);
  if (!original) return { error: "not_found" };
  const limit = limitError(scene);
  if (limit) return limit;
  const copy = cloneJson(original);
  copy.id = directorNewObjectId(random);
  copy.name = duplicateName(objects, original.name);
  copy.transform.position = { ...copy.transform.position, x: copy.transform.position.x + 0.5 };
  copy.source = "user";
  copy.locked = false;
  return { scene: { ...scene, objects: [...objects, copy] }, object: copy };
}

/**
 * 删除对象。删的是输出机位时，自动改选剩下的第一个机位，一个都不剩时置 null（DR-F-24）。
 * 挂在它身上的时间轴轨道一并删掉：留着的话场景本身就不合法了（轨道指向不存在的对象），保存会被拒。
 */
export function directorRemoveObject(scene: DirectorScene, id: string): DirectorScene {
  const objects = scene.objects ?? [];
  if (!objects.some((o) => o.id === id)) return scene;
  const rest = objects.filter((o) => o.id !== id);
  const next: DirectorScene = { ...scene, objects: rest };
  if (scene.outputCameraId === id) next.outputCameraId = rest.find((o) => o.kind === "camera")?.id ?? null;
  const tracks = scene.timeline?.tracks;
  if (scene.timeline && tracks?.some((t) => t.objectId === id)) {
    next.timeline = { ...scene.timeline, tracks: tracks.filter((t) => t.objectId !== id) };
  }
  return next;
}

function updateObject(scene: DirectorScene, id: string, patch: (o: DirectorObject) => DirectorObject): DirectorScene {
  const objects = scene.objects ?? [];
  const i = objects.findIndex((o) => o.id === id);
  if (i < 0) return scene;
  const current = objects[i] as DirectorObject;
  const updated = patch(current);
  if (updated === current) return scene;
  const nextObjects = objects.slice();
  nextObjects[i] = updated;
  return { ...scene, objects: nextObjects };
}

/** 重命名（DR-F-14）：去掉首尾空白、截断到 20 字；空名称不改（名称是必填项）。 */
export function directorRenameObject(scene: DirectorScene, id: string, name: string): DirectorScene {
  const trimmed = truncateName(name.trim());
  if (trimmed === "") return scene;
  return updateObject(scene, id, (o) => (o.name === trimmed ? o : { ...o, name: trimmed }));
}

export function directorSetVisible(scene: DirectorScene, id: string, visible: boolean): DirectorScene {
  return updateObject(scene, id, (o) => ((o.visible ?? true) === visible ? o : { ...o, visible }));
}

export function directorSetLocked(scene: DirectorScene, id: string, locked: boolean): DirectorScene {
  return updateObject(scene, id, (o) => ((o.locked ?? false) === locked ? o : { ...o, locked }));
}

/** 设输出机位（同一时间只有一个，DR-F-24）。不是机位的 ID 不改；传 null 表示不输出。 */
export function directorSetOutputCamera(scene: DirectorScene, id: string | null): DirectorScene {
  if (id !== null && !(scene.objects ?? []).some((o) => o.id === id && o.kind === "camera")) return scene;
  if (scene.outputCameraId === id) return scene;
  return { ...scene, outputCameraId: id };
}

/**
 * 用户改动了 AI 生成的对象时，把它记成用户对象（DR-F-105）：之后的 AI 重新搭景不再替换它。
 * store 在每次用户提交时调用；不是 AI 对象时原样返回同一个对象。
 */
export function directorTouch(object: DirectorObject): DirectorObject {
  return object.source === "ai" ? { ...object, source: "user" } : object;
}

/** 单阵群众人数上限（DR-PARAM-09）：界面改行列数时先问这里。 */
export function directorCrowdSizeError(rows: number, cols: number): DirectorLimitError | null {
  return rows * cols > DIRECTOR_LIMITS.maxCrowdSize ? { error: "limit", what: "crowd", limit: DIRECTOR_LIMITS.maxCrowdSize } : null;
}

/** 正文大小上限（512 KiB）：store 保存前问这里，超出时不发请求（服务端也会回 41301）。 */
export function directorSceneSizeError(scene: DirectorScene): DirectorLimitError | null {
  return directorSceneBytes(scene) > DIRECTOR_LIMITS.maxSceneBytes
    ? { error: "limit", what: "sceneBytes", limit: DIRECTOR_LIMITS.maxSceneBytes }
    : null;
}

// 权限判断（`directorCanEdit`）在 `utils/directorRole.ts`：画布上也要用，放在这里会把整个场景模块带进画布页首屏。

// ---------------------------------------------------------------- 视口编辑（DRT-13）

/**
 * 统一成等比缩放（DR-F-33：角色只能等比缩放）：取**偏离 1 最多**的那一轴的值，三轴都用它。
 * 用于修正旧数据里三轴不等的角色，以及属性面板只显示一个缩放值。偏离一样多时按 x、y、z 的顺序取第一个；
 * 非有限值或非正数不参与（当作 1），全都不参与时回 1。
 */
export function directorUniformScale(scale: DirectorVec3): DirectorVec3 {
  let best = 1;
  let deviation = 0;
  for (const v of [scale.x, scale.y, scale.z]) {
    if (!Number.isFinite(v) || v <= 0) continue;
    const d = Math.abs(v - 1);
    if (d > deviation) {
      deviation = d;
      best = v;
    }
  }
  return { x: best, y: best, z: best };
}

/** 视口里能不能点选、拖动（DR-F-10）：未锁定且可见。场景树里照样能选中锁定的对象（属性面板只读）。 */
export function directorIsPickable(object: Pick<DirectorObject, "locked" | "visible">): boolean {
  return object.locked !== true && object.visible !== false;
}

// ---------------------------------------------------------------- 属性编辑（DRT-15）

/** 按 ID 取对象；不存在时为 undefined。 */
export function directorFindObject(scene: DirectorScene | null | undefined, id: string | null | undefined): DirectorObject | undefined {
  if (!scene || !id) return undefined;
  return (scene.objects ?? []).find((o) => o.id === id);
}

function sameJson(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

function replaceObject(scene: DirectorScene, id: string, update: (object: DirectorObject) => DirectorObject): DirectorScene {
  const objects = scene.objects ?? [];
  const index = objects.findIndex((o) => o.id === id);
  if (index < 0) return scene;
  const before = objects[index] as DirectorObject;
  const after = update(before);
  if (after === before || sameJson(after, before)) return scene;
  return { ...scene, objects: objects.map((o, i) => (i === index ? after : o)) };
}

/**
 * 换掉一个对象的变换（视口拖拽 / 走位结束、属性面板的数值）。transform 里的未知字段保留。
 * ⚠️ 值没变时返回**同一个**场景：store 的 `commit` 据此不记撤销、不排保存（按住不动就松开不算一步）。
 */
export function directorSetTransform(
  scene: DirectorScene,
  id: string,
  transform: { position: DirectorVec3; rotation: DirectorVec3; scale: DirectorVec3 }
): DirectorScene {
  return replaceObject(scene, id, (object) => ({
    ...object,
    transform: {
      ...object.transform,
      position: { ...transform.position },
      rotation: { ...transform.rotation },
      scale: { ...transform.scale }
    }
  }));
}

/** 对象子结构的键（属性面板按对象种类改的那一段）。 */
export type DirectorSubKey = "character" | "camera" | "prop" | "crowd";

/** 改对象子结构里的若干字段：浅合并，未知字段保留；子结构缺失时按目录默认值补齐。值都没变时返回同一个场景。 */
export function directorPatchObject(scene: DirectorScene, id: string, key: DirectorSubKey, patch: Record<string, unknown>): DirectorScene {
  const defaults = DIRECTOR_OBJECT_DEFAULTS[key] as Record<string, unknown>;
  return replaceObject(scene, id, (object) => {
    const current = (object[key] && typeof object[key] === "object" ? object[key] : cloneJson(defaults)) as Record<string, unknown>;
    return { ...object, [key]: { ...current, ...patch } };
  });
}

/** 场景级设置里能整段改的键（DR-F-60~63、DR-F-31 / 32 / 44）。 */
export type DirectorSettingsKey = "stage" | "background" | "ground" | "view";

/** 改场景级设置的一段：浅合并，缺失时按默认值补齐，未知字段保留。值都没变时返回同一个场景。 */
export function directorPatchSettings(scene: DirectorScene, key: DirectorSettingsKey, patch: Record<string, unknown>): DirectorScene {
  const defaults = DIRECTOR_OBJECT_DEFAULTS.scene[key] as Record<string, unknown>;
  const current = (scene[key] && typeof scene[key] === "object" ? scene[key] : cloneJson(defaults)) as Record<string, unknown>;
  const next = { ...current, ...patch };
  if (sameJson(next, current) && scene[key] !== undefined) return scene;
  return { ...scene, [key]: next };
}

/** 改主光 / 补光（DR-F-63）：只动那一盏灯，另一盏原样。 */
export function directorPatchLight(scene: DirectorScene, which: "key" | "fill", patch: Record<string, unknown>): DirectorScene {
  const defaults = DIRECTOR_OBJECT_DEFAULTS.scene.lights as Record<string, Record<string, unknown>>;
  const lights = (scene.lights && typeof scene.lights === "object" ? scene.lights : cloneJson(defaults)) as Record<string, unknown>;
  const current = (lights[which] && typeof lights[which] === "object" ? lights[which] : cloneJson(defaults[which])) as Record<string, unknown>;
  const next = { ...current, ...patch };
  if (sameJson(next, current) && scene.lights !== undefined) return scene;
  return { ...scene, lights: { ...lights, [which]: next } };
}

// ---------------------------------------------------------------- 群众阵列与上限（DRT-22）

/** 上限错误给用户看的话（DR-F-13）：说清是哪类上限、上限是多少。 */
export function directorLimitText(error: DirectorLimitError): string {
  if (error.what === "crowd") return `单阵最多 ${error.limit} 人`;
  if (error.what === "objects") return `场景里最多 ${error.limit} 个对象`;
  return `场景内容已到上限（${Math.round(error.limit / 1024)} KiB），请先删掉一些对象`;
}

/**
 * 还能不能再加一个对象（DR-F-13，DR-PARAM-09）：对象总数到 200 时不能再加任何对象；
 * 加群众（或把群众改大）时另看单阵人数 `crowdSize`，超过 100 拒绝。界面据 `reason` 说清上限。
 */
export function directorCanAddObject(
  scene: DirectorScene,
  kind: DirectorObjectKind,
  crowdSize?: number
): { ok: true } | { ok: false; reason: string; error: DirectorLimitError } {
  const objects = limitError(scene);
  if (objects) return { ok: false, reason: directorLimitText(objects), error: objects };
  if (kind === "crowd" && crowdSize !== undefined && crowdSize > DIRECTOR_LIMITS.maxCrowdSize) {
    const error: DirectorLimitError = { error: "limit", what: "crowd", limit: DIRECTOR_LIMITS.maxCrowdSize };
    return { ok: false, reason: directorLimitText(error), error };
  }
  return { ok: true };
}

/**
 * 以整数为种子的伪随机数（mulberry32），每次调用返回 [0, 1) 里的下一个数。
 * 群众错位要「同一种子结果不变」（DR-F-50）：不能用 `Math.random`，重开编辑器、换一台机器都得排出同一个阵。
 */
export function directorSeededRandom(seed: number): () => number {
  let a = (Number.isFinite(seed) ? Math.trunc(seed) : 0) >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** 一个群众实例在阵列坐标系里的位置（米）与朝向（度，绕 Y 轴）。地面高度由阵列整体决定，这里不给 y。 */
export type DirectorCrowdSlot = { x: number; z: number; yaw: number };

/** 错位的幅度：位移不超过 ±0.25 × 间距，朝向不超过 ±30°（DR-F-50）。 */
const CROWD_JITTER_SHIFT = 0.25;
const CROWD_JITTER_YAW = 30;

/**
 * 群众阵列的布局（DR-F-50、DRD-37）：rows × cols 的网格，按 `spacing` 排列、居中于对象原点；
 * 行沿 Z（纵深）、列沿 X，按行优先编号。`jitter` 打开时用以 `seed` 为种子的伪随机数给每个实例加错位与朝向，
 * 同一种子结果不变；关着时整齐排列、都朝前（yaw = 0）。
 * 行列夹在 1~10（与场景校验一致），间距不是有限正数时按默认值 —— 引擎拿到的可能是编辑中的数据，不能因此崩掉。
 */
export function directorCrowdLayout(crowd: { rows?: number; cols?: number; spacing?: number; jitter?: boolean; seed?: number }): DirectorCrowdSlot[] {
  const d = DIRECTOR_OBJECT_DEFAULTS.crowd;
  const clampInt = (v: unknown, fallback: number, max: number) => {
    const n = typeof v === "number" && Number.isFinite(v) ? Math.trunc(v) : fallback;
    return Math.min(max, Math.max(1, n));
  };
  const rows = clampInt(crowd.rows, d.rows, DIRECTOR_LIMITS.maxCrowdRows);
  const cols = clampInt(crowd.cols, d.cols, DIRECTOR_LIMITS.maxCrowdCols);
  const spacing = typeof crowd.spacing === "number" && Number.isFinite(crowd.spacing) && crowd.spacing > 0 ? crowd.spacing : d.spacing;
  const random = crowd.jitter === true ? directorSeededRandom(crowd.seed ?? d.seed) : null;
  const slots: DirectorCrowdSlot[] = [];
  for (let r = 0; r < rows; r++) {
    for (let c = 0; c < cols; c++) {
      let x = (c - (cols - 1) / 2) * spacing;
      let z = (r - (rows - 1) / 2) * spacing;
      let yaw = 0;
      if (random) {
        x += (random() * 2 - 1) * CROWD_JITTER_SHIFT * spacing;
        z += (random() * 2 - 1) * CROWD_JITTER_SHIFT * spacing;
        yaw = (random() * 2 - 1) * CROWD_JITTER_YAW;
      }
      slots.push({ x, z, yaw });
    }
  }
  return slots;
}
