/**
 * 引擎对外的事件（dev/03 §4，DRT-12 / 13）。引擎只投影场景：用户在视口里的操作结束时发事件，
 * 由 store 改数据、记一步撤销，再经 `setScene` 回灌（DRD-36）。拖动、走位途中的临时状态不发。
 */
import type { DirectorBoneRotation } from "~/utils/directorRig";
import type { DirectorObjectKind, DirectorVec3 } from "~/utils/directorScene";

/** 一个对象的变换（旋转为欧拉角，单位度，顺序 XYZ）。与场景正文的 `transform` 同形。 */
export type DirectorTransformPayload = { position: DirectorVec3; rotation: DirectorVec3; scale: DirectorVec3 };

export type DirectorTransformMode = "translate" | "rotate" | "scale";

export interface DirectorEngineEvents {
  /** 第一份场景（含素模）加载完毕。 */
  ready: undefined;
  /** 视口里点选了对象；点空白处为 null（DRT-13）。 */
  select: { id: string | null; via: "viewport" };
  /** 自由走位结束（松开按键 300ms 后、拖动转向结束时）发一次；store 记一步（DR-F-25）。 */
  "camera-moved": { id: string; transform: DirectorTransformPayload };
  /** 一次拖拽结束发恰好一次（DR-F-34，DRT-13）。 */
  "transform-end": { id: string; transform: DirectorTransformPayload };
  /** 角色与机位不能切到缩放（DR-F-33）：界面提示「角色请用身高调整」。 */
  "mode-rejected": { id: string; kind: DirectorObjectKind; mode: DirectorTransformMode };
  /** WebGL 上下文丢失：编辑器显示「图形环境已重置」与「重新加载编辑器」。 */
  "context-lost": undefined;
  /** 要告诉用户的一句话（DRT-28：泼溅加载失败、贴地点太稀退回地面）。 */
  notice: { level: "info" | "error"; text: string };
  /**
   * 时间轴（DRT-24）：播放中每进一帧发一次，开始 / 停止播放时也发（`playing` 变了）。外壳据此移动播放头；
   * 拖动播放头（`seek`）不发 —— 那是外壳自己发起的。
   */
  frame: { frame: number; playing: boolean };
  /** 关节模式下在视口里点了关节小球；点空白处 `bone` 为 null（DRT-21）。 */
  "joint-select": { id: string; bone: string | null };
  /**
   * 一次姿势编辑结束（DRT-21，DR-F-34 / 43）：拖关节（FK）、拖手脚末端（IK）、移骨盆，或脚底锁定时拖动 / 转动角色。
   * - `boneOverrides`：整组新的骨骼覆盖（相对 rest 的欧拉角，度）；
   * - `transform`：角色本身动了时才有（移骨盆、锁脚拖角色）；锁着脚时竖直方向的移动由腿吸收、不改 root 高度；
   * - `footLock` / `released`：腿伸直也够不着锁定点、自动解锁了哪只脚（界面提示「脚已解锁」）。
   */
  "pose-end": {
    id: string;
    boneOverrides: Record<string, DirectorBoneRotation>;
    transform?: DirectorTransformPayload;
    footLock?: { left: boolean; right: boolean };
    released?: Array<"left" | "right">;
  };
}

type Handler<T> = (payload: T) => void;

/** 极简的类型化事件分发。监听器抛错不影响其余监听器（只打一条错误）。 */
export class DirectorEmitter<E extends object> {
  private handlers = new Map<keyof E, Set<Handler<never>>>();

  on<K extends keyof E>(type: K, handler: Handler<E[K]>): () => void {
    let set = this.handlers.get(type);
    if (!set) {
      set = new Set();
      this.handlers.set(type, set);
    }
    set.add(handler as Handler<never>);
    return () => set.delete(handler as Handler<never>);
  }

  emit<K extends keyof E>(type: K, payload: E[K]): void {
    for (const handler of this.handlers.get(type) ?? []) {
      try {
        (handler as Handler<E[K]>)(payload);
      } catch (err) {
        console.error(`[director] 事件 ${String(type)} 的监听器出错`, err);
      }
    }
  }

  clear(): void {
    this.handlers.clear();
  }
}
