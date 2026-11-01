/**
 * 地面吸附（DR-F-32，DR-AC-04，dev/03 §4.2 GroundSnap）：角色脚底、道具底面贴到正下方的表面。
 *
 * 在 stage 局部坐标里做（地面、道具都在 stage 里，场景整体变换倾斜时「正下方」也跟着倾斜）：
 * 从对象当前水平位置上方 50 m 向下打射线，取第一个命中的表面 —— 地面（解析平面 y = 地面高度）、道具（BVH）、
 * 以及注册进来的额外目标；泼溅（DRT-28）没有网格可打，按脚下半径 0.5 m 内的中心点拟合平面，与射线结果取较高者。
 * 打不中时返回 null，调用方保持原高度，不掉到 −∞。
 *
 * ⚠️ 射线必须排除对象自己与它的子物体：从对象内部打出的射线先打中自己，贴上去之后对象一路往上「爬」。
 * ⚠️ 地面隐藏时照样贴（隐藏的是画面里的地面，人站在哪个高度不变）。
 */
import { Matrix4, Raycaster, Vector3, type Group, type Object3D } from "three";

const PROBE_HEIGHT = 50;

const _origin = new Vector3();
const _dir = new Vector3();
const _inv = new Matrix4();
const _p = new Vector3();

function isWithin(object: Object3D, root: Object3D): boolean {
  for (let o: Object3D | null = object; o; o = o.parent) if (o === root) return true;
  return false;
}

/** 泼溅表面（DRT-28）：stage 局部坐标 (x, z) 处的高度；不在泼溅范围内为 null，点太稀为 `{sparse}`。 */
export type GroundSnapSplatSurface = (x: number, z: number, nearY: number) => { height: number } | { sparse: true } | null;

export class GroundSnap {
  private readonly raycaster = new Raycaster();
  private readonly extra = new Set<Object3D>();
  /** 最近一次贴地落在了泼溅范围内、但那里的点太稀而退回地面（拖完由引擎提示一次，DR-F-141）。 */
  private splatFallback = false;

  constructor(
    private readonly stage: Group,
    private readonly groundHeight: () => number,
    private readonly surfaces: () => Object3D[],
    private readonly splatSurface: GroundSnapSplatSurface | null = null
  ) {
    this.raycaster.layers.enableAll();
    this.raycaster.firstHitOnly = true;
  }

  /** 取走「退回地面」的标记（只提示一次）。 */
  takeSplatFallback(): boolean {
    const was = this.splatFallback;
    this.splatFallback = false;
    return was;
  }

  /** 注册额外的贴地目标（泼溅代理面，DRT-28）；返回注销函数。 */
  register(target: Object3D): () => void {
    this.extra.add(target);
    return () => this.extra.delete(target);
  }

  /** `root`（stage 的直接子物体）正下方第一个表面的高度（stage 局部 y）；打不中回 null。 */
  surfaceBelow(root: Object3D): number | null {
    const top = root.position.y + PROBE_HEIGHT;
    const ground = this.groundHeight();
    let best: number | null = top >= ground ? ground : null;

    const targets = [...this.surfaces(), ...this.extra].filter((t) => !isWithin(t, root));
    if (targets.length) {
      this.stage.updateWorldMatrix(true, true);
      _origin.set(root.position.x, top, root.position.z).applyMatrix4(this.stage.matrixWorld);
      _dir.set(0, -1, 0).transformDirection(this.stage.matrixWorld);
      this.raycaster.set(_origin, _dir);
      const hit = this.raycaster.intersectObjects(targets, true)[0];
      if (hit) {
        _inv.copy(this.stage.matrixWorld).invert();
        const y = _p.copy(hit.point).applyMatrix4(_inv).y;
        // 第一个命中就是最高的那个表面；地面比它高（道具埋在地下）时以地面为准。
        if (best === null || y > best) best = y;
      }
    }
    // 泼溅表面（DRT-28）：拟合出来的高度与上面的表面同样按「最高者」取；点太稀时记下来，拖完提示一次
    // 标记只看这一次（拖动途中经过稀的地方、最后落在别处的话不提示）
    const splat = this.splatSurface?.(root.position.x, root.position.z, root.position.y) ?? null;
    this.splatFallback = splat !== null && !("height" in splat);
    if (splat && "height" in splat && (best === null || splat.height > best)) best = splat.height;
    return best;
  }
}
