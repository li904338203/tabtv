/**
 * 高斯泼溅背景（DRT-28，DR-F-140~144，dev/03 §11）。每个场景最多一个，放在 stage 里（跟着场景整体变换，角色站在上面）。
 *
 * - Spark（`@sparkjsdev/spark` 2.4.0，MIT）第一次遇到泼溅时才 import，不进编辑器首屏；
 * - 受保护媒体先经 `fetchMediaBlob` 取成字节再交给 `SplatMesh({fileBytes})`（DRD-40）：把受保护地址交给它去取会 401；
 * - 一个渲染器只挂一个 `SparkRenderer`（Spark #107：多实例出错），它的 `onDirty` 接引擎的 `invalidate` ——
 *   引擎按需渲染，排序是异步的，排完不再画一帧的话视口停在上一次的顺序；
 * - 不参与点选（`raycastable: false`，Spark #468 精确拾取未做）：泼溅只当背景，变换在面板里调；
 * - 贴地：加载后把不透明度 ≥ 0.2 的中心点换到 stage 局部坐标，按 0.5 m 的格子建索引，`surfaceAt` 取脚下半径 0.5 m 内的点，
 *   挑出最密的那一层（`directorSplatGroundBand`）再拟合平面（`directorFitGroundPlane`）；变换改了就作废重建（下一次贴地时再建）；
 * - 截图与预演导出前 `ready(camera)`：先用截图机位让 Spark 更新并排序，等 `sorting` / `sortDirty` 都落下、且排序视点
 *   就是这个机位，再出图（DR-F-142）。不等的话截图用的是视口机位的排序，近处的泼溅前后颠倒；
 * - 深度图用覆盖材质渲染，泼溅（半透明的高斯，没有可靠的深度）在那时隐藏：`setRendered(false)`。
 */
import { Matrix4, Vector3, type Camera, type WebGLRenderer } from "three";
import type { SparkRenderer, SplatMesh } from "@sparkjsdev/spark";
import { DIRECTOR_SPLAT_MAX_RESIDUAL, directorFitGroundPlane, directorSplatGroundBand, type DirectorSplat } from "~/utils/directorSplat";
import { DEG, type EngineContext } from "./context";

type SparkModule = typeof import("@sparkjsdev/spark");

/** 贴地取点的半径（米）。 */
const SNAP_RADIUS = 0.5;
const CELL = 0.5;
/** 当成地表的最低不透明度：更淡的多是飘在空中的雾状点。 */
const MIN_OPACITY = 0.2;
/** 等排序完成的上限：大场景第一次排序也就几百毫秒，超过它照常出图并告警，不让截图卡死。 */
const SORT_WAIT_MS = 5000;

export type DirectorSplatSurface = { height: number; residual: number; count: number } | { sparse: true; count: number } | null;

export class Splats {
  private module: Promise<SparkModule> | null = null;
  private spark: SparkRenderer | null = null;
  private mesh: SplatMesh | null = null;
  private mediaId: string | null = null;
  private token = 0;
  private loading: Promise<void> | null = null;
  private disposed = false;
  private visible = true;
  /** 最近一次 `apply` 的泼溅：加载期间变换又改了的话，加载完按最新的摆。 */
  private latest: DirectorSplat | null = null;
  /** stage 局部坐标的中心点（x, y, z 连续存放）与格子索引；变换改了置空，用到时再建。 */
  private centers: Float32Array | null = null;
  private grid: Map<string, number[]> | null = null;
  private localCenters: Float32Array | null = null;

  constructor(
    private readonly ctx: EngineContext,
    private readonly renderer: WebGLRenderer,
    private readonly fetchMediaBlob: ((mediaId: string) => Promise<Blob>) | null,
    private readonly onError: (message: string) => void
  ) {
    ctx.disposer.defer(() => {
      this.disposed = true;
      this.token++;
      this.release();
      this.spark?.removeFromParent();
      this.spark?.dispose();
      this.spark = null;
    });
  }

  /** 按场景的 `splat` 段更新：换文件才重新加载，变换与显示即时生效。 */
  apply(splat: DirectorSplat | null): void {
    this.latest = splat;
    const mediaId = splat?.mediaId ?? null;
    if (mediaId !== this.mediaId) {
      this.mediaId = mediaId;
      const token = ++this.token;
      this.release();
      if (mediaId) {
        const loading = this.load(mediaId, token).finally(() => {
          if (this.loading === loading) this.loading = null;
        });
        this.loading = loading;
      }
    }
    this.visible = splat?.visible !== false;
    if (this.mesh && splat) this.applyTransform(this.mesh, splat);
    this.ctx.invalidate();
  }

  /** 正在加载的泼溅（截图前要等它）；没有在加载时立即 resolve。 */
  whenIdle(): Promise<void> {
    return this.loading ?? Promise.resolve();
  }

  /** 有泼溅而且在显示（贴地、截图等待只看显示中的）。 */
  get active(): boolean {
    return Boolean(this.mesh && this.visible);
  }

  /** 深度图渲染时隐藏（覆盖材质画不了泼溅）；结束后恢复。 */
  setRendered(on: boolean): void {
    if (this.spark) this.spark.visible = on;
  }

  /**
   * 等 Spark 以 `camera` 为视点排序完成（DR-F-142）。泼溅没加载或隐藏时立即返回。
   * 返回 false 表示等到上限也没排完（照常出图，调用方可以告警）。
   */
  async ready(camera: Camera): Promise<boolean> {
    const spark = this.spark;
    const mesh = this.mesh;
    if (!spark || !mesh || !this.visible) return true;
    await mesh.initialized;
    camera.updateMatrixWorld();
    const position = camera.getWorldPosition(new Vector3());
    const direction = camera.getWorldDirection(new Vector3());
    const deadline = performance.now() + SORT_WAIT_MS;
    await spark.update({ scene: this.ctx.scene, camera });
    for (;;) {
      const settled = !spark.sorting && !spark.sortDirty
        && spark.sortedCenter.distanceTo(position) < 1e-3 && spark.sortedDir.dot(direction) > 0.999;
      if (settled) return true;
      if (performance.now() > deadline) return false;
      await new Promise((resolve) => setTimeout(resolve, 16));
      // 排序被节流时 Spark 先返回、由定时器补跑；视点被视口那一帧抢走时重新以截图机位更新一次
      if (!spark.sorting && (spark.sortDirty || spark.sortedCenter.distanceTo(position) >= 1e-3)) {
        await spark.update({ scene: this.ctx.scene, camera });
      }
    }
  }

  /**
   * 脚下 (x, z)（stage 局部坐标）的泼溅表面：取半径 0.5 m 内、高度在 [nearY − 3, nearY + 1] 的中心点拟合平面。
   * - 那里没有泼溅点：返回 null（不在泼溅范围内，按地面与道具贴）；
   * - 点太少或残差太大：返回 `{sparse: true}`，调用方退回地面并提示「这里的泼溅太稀」（DR-F-141）。
   */
  surfaceAt(x: number, z: number, nearY: number): DirectorSplatSurface {
    if (!this.active) return null;
    const index = this.index();
    if (!index) return null;
    const { centers, grid } = index;
    const points: Array<{ x: number; y: number; z: number }> = [];
    const r2 = SNAP_RADIUS * SNAP_RADIUS;
    const cx0 = Math.floor((x - SNAP_RADIUS) / CELL);
    const cx1 = Math.floor((x + SNAP_RADIUS) / CELL);
    const cz0 = Math.floor((z - SNAP_RADIUS) / CELL);
    const cz1 = Math.floor((z + SNAP_RADIUS) / CELL);
    for (let cx = cx0; cx <= cx1; cx++) {
      for (let cz = cz0; cz <= cz1; cz++) {
        for (const i of grid.get(`${cx},${cz}`) ?? []) {
          const px = centers[i * 3] as number;
          const py = centers[i * 3 + 1] as number;
          const pz = centers[i * 3 + 2] as number;
          if ((px - x) ** 2 + (pz - z) ** 2 > r2 || py < nearY - 3 || py > nearY + 1) continue;
          points.push({ x: px, y: py, z: pz });
        }
      }
    }
    if (!points.length) return null;
    // 先挑出脚下最密的那一层（石头、墙根的点不混进来），再拟合
    const fit = directorFitGroundPlane(directorSplatGroundBand(points), { x, z });
    if (!fit || fit.residual > DIRECTOR_SPLAT_MAX_RESIDUAL) return { sparse: true, count: points.length };
    return { height: fit.height, residual: fit.residual, count: fit.count };
  }

  /** 调试与自查：泼溅点数、包围范围（stage 局部坐标）。 */
  stats(): { splats: number; indexed: number } | null {
    if (!this.mesh) return null;
    return { splats: this.localCenters ? this.localCenters.length / 3 : 0, indexed: this.centers ? this.centers.length / 3 : 0 };
  }

  private applyTransform(mesh: SplatMesh, splat: DirectorSplat): void {
    mesh.position.set(splat.position.x, splat.position.y, splat.position.z);
    mesh.rotation.set(splat.rotation.x * DEG, splat.rotation.y * DEG, splat.rotation.z * DEG, "XYZ");
    mesh.scale.setScalar(splat.scale);
    mesh.visible = splat.visible;
    mesh.updateMatrix();
    // 变换变了，贴地索引作废（下一次贴地时按新的变换重建）
    this.centers = null;
    this.grid = null;
  }

  private async load(mediaId: string, token: number): Promise<void> {
    if (!this.fetchMediaBlob) return;
    try {
      const [spark, blob] = await Promise.all([this.sparkModule(), this.fetchMediaBlob(mediaId)]);
      if (token !== this.token || this.disposed) return;
      const bytes = new Uint8Array(await blob.arrayBuffer());
      if (token !== this.token || this.disposed) return;
      this.ensureRenderer(spark);
      const mesh = new spark.SplatMesh({ fileBytes: bytes, raycastable: false });
      await mesh.initialized;
      if (token !== this.token || this.disposed) {
        mesh.dispose();
        return;
      }
      // 中心点（对象局部坐标）记一份：变换改了不用重新遍历泼溅，只重新乘矩阵
      const local: number[] = [];
      mesh.forEachSplat((_i, center, _scales, _q, opacity) => {
        if (opacity >= MIN_OPACITY) local.push(center.x, center.y, center.z);
      });
      this.localCenters = Float32Array.from(local);
      this.mesh = mesh;
      mesh.matrixAutoUpdate = false;
      if (this.latest) this.applyTransform(mesh, this.latest);
      this.ctx.stage.add(mesh);
      this.ctx.invalidate();
    } catch (err) {
      if (token !== this.token) return;
      console.warn(`[director] 泼溅 ${mediaId} 加载失败`, err);
      this.onError("泼溅文件加载失败：文件可能已损坏，请重新导入");
    }
  }

  private sparkModule(): Promise<SparkModule> {
    if (!this.module) {
      const loading = import("@sparkjsdev/spark");
      loading.catch(() => {
        if (this.module === loading) this.module = null;
      });
      this.module = loading;
    }
    return this.module;
  }

  private ensureRenderer(spark: SparkModule): void {
    if (this.spark) return;
    this.spark = new spark.SparkRenderer({ renderer: this.renderer, onDirty: () => this.ctx.invalidate() });
    this.spark.name = "spark";
    this.ctx.scene.add(this.spark);
  }

  private index(): { centers: Float32Array; grid: Map<string, number[]> } | null {
    const mesh = this.mesh;
    const local = this.localCenters;
    if (!mesh || !local) return null;
    if (this.centers && this.grid) return { centers: this.centers, grid: this.grid };
    const matrix = new Matrix4().copy(mesh.matrix);
    const v = new Vector3();
    const centers = new Float32Array(local.length);
    const grid = new Map<string, number[]>();
    for (let i = 0; i < local.length / 3; i++) {
      v.set(local[i * 3] as number, local[i * 3 + 1] as number, local[i * 3 + 2] as number).applyMatrix4(matrix);
      centers[i * 3] = v.x;
      centers[i * 3 + 1] = v.y;
      centers[i * 3 + 2] = v.z;
      const key = `${Math.floor(v.x / CELL)},${Math.floor(v.z / CELL)}`;
      const cell = grid.get(key);
      if (cell) cell.push(i);
      else grid.set(key, [i]);
    }
    this.centers = centers;
    this.grid = grid;
    return { centers, grid };
  }

  private release(): void {
    if (this.mesh) {
      this.mesh.removeFromParent();
      this.mesh.dispose();
      this.mesh = null;
    }
    this.localCenters = null;
    this.centers = null;
    this.grid = null;
    this.ctx.invalidate();
  }
}
