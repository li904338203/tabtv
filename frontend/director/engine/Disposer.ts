/**
 * 引擎资源的统一释放（dev/03 §4.2，DR-AC-02、DR-N-04）。
 *
 * 各模块创建的几何、材质、纹理、渲染目标都在这里登记，关闭时一次释放；最后再遍历一遍场景图兜底，
 * 漏登记的也能被收走。之后由 RendererHost `renderer.dispose()` + `forceContextLoss()`。
 *
 * ⚠️ 共享的几何、材质只释放一次：用 Set 去重。three 的释放靠 dispose 事件，重复调用虽然不会重复减计数，
 *    但「一份资源在两处被释放」本身说明所有权没理清，排查内存时会被它误导。
 * ⚠️ BVH（DRT-13）随几何一起丢掉（`boundsTree = undefined`）：它只占 JS 内存，不进 `renderer.info`，
 *    漏了的话 DR-AC-02 的计数照样归零，而堆一直在涨。
 * ⚠️ 骨架（Skeleton）也要单独释放：每个素模克隆一份骨骼纹理（`boneTexture`），计入 textures，
 *    而它不挂在几何或材质上，遍历几何、材质收不到它。
 */
import type { BufferGeometry, Material, Object3D, Skeleton, SkinnedMesh, Texture, WebGLRenderTarget } from "three";

type WithBoundsTree = BufferGeometry & { boundsTree?: unknown };

function texturesOf(material: Material): Texture[] {
  const out: Texture[] = [];
  for (const value of Object.values(material as unknown as Record<string, unknown>)) {
    if (value && typeof value === "object" && (value as { isTexture?: boolean }).isTexture) out.push(value as Texture);
  }
  return out;
}

export class Disposer {
  private readonly geometries = new Set<BufferGeometry>();
  private readonly materials = new Set<Material>();
  private readonly textures = new Set<Texture>();
  private readonly targets = new Set<WebGLRenderTarget>();
  private readonly skeletons = new Set<Skeleton>();
  private readonly callbacks: Array<() => void> = [];
  private disposed = false;

  geometry<T extends BufferGeometry>(g: T): T {
    this.geometries.add(g);
    return g;
  }

  material<T extends Material>(m: T): T {
    this.materials.add(m);
    return m;
  }

  texture<T extends Texture>(t: T): T {
    this.textures.add(t);
    return t;
  }

  target<T extends WebGLRenderTarget>(rt: T): T {
    this.targets.add(rt);
    return rt;
  }

  skeleton<T extends Skeleton>(s: T): T {
    this.skeletons.add(s);
    return s;
  }

  /** 关闭时要做的额外清理（移除监听、DOM、控件），按登记的**相反**顺序执行。 */
  defer(fn: () => void): void {
    this.callbacks.push(fn);
  }

  /** 提前释放一张纹理（换全景、切回纯色时用），并不再跟踪它。 */
  releaseTexture(t: Texture | null | undefined): void {
    if (!t) return;
    this.textures.delete(t);
    t.dispose();
  }

  /** 提前释放一份几何（对象被删除、参数变化重建时用）。 */
  releaseGeometry(g: BufferGeometry | null | undefined): void {
    if (!g) return;
    this.geometries.delete(g);
    (g as WithBoundsTree).boundsTree = undefined;
    g.dispose();
  }

  /** 提前释放一副骨架（角色被删除、换素模时用）。 */
  releaseSkeleton(s: Skeleton | null | undefined): void {
    if (!s) return;
    this.skeletons.delete(s);
    s.dispose();
  }

  /** 把场景图里还挂着的几何、材质、纹理、骨架收进来（兜底：漏登记的也会被释放）。 */
  collect(root: Object3D): void {
    root.traverse((object) => {
      const o = object as Object3D & { geometry?: BufferGeometry; material?: Material | Material[] };
      if ((object as SkinnedMesh).isSkinnedMesh) this.skeletons.add((object as SkinnedMesh).skeleton);
      if (o.geometry) this.geometries.add(o.geometry);
      const materials = Array.isArray(o.material) ? o.material : o.material ? [o.material] : [];
      for (const m of materials) this.materials.add(m);
    });
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const fn of this.callbacks.splice(0).reverse()) {
      try {
        fn();
      } catch (err) {
        console.error("[director] 释放回调出错", err);
      }
    }
    for (const m of this.materials) for (const t of texturesOf(m)) this.textures.add(t);
    for (const g of this.geometries) {
      (g as WithBoundsTree).boundsTree = undefined;
      g.dispose();
    }
    for (const m of this.materials) m.dispose();
    for (const t of this.textures) t.dispose();
    for (const rt of this.targets) rt.dispose();
    for (const sk of this.skeletons) sk.dispose();
    this.geometries.clear();
    this.materials.clear();
    this.textures.clear();
    this.targets.clear();
    this.skeletons.clear();
  }
}
