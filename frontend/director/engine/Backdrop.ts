/**
 * 背景、地面、网格与坐标轴（DR-F-61 / 62 / 64，DR-N-05，DRD-40，dev/03 §6.6）。
 *
 * - 纯色背景是清屏色（`background.color`）；全景是一个以世界原点为球心的内翻球，半径与水平旋转取 `background.panorama`，
 *   不随场景整体变换（场景整体变换正是用来把场景对齐到全景上的，DR-F-60）；
 * - 地面在 stage 里（跟着场景整体变换），可隐藏、调透明度与高度，属于画面内容（layer 0）；
 * - 网格与坐标轴是编辑辅助（layer 1），跟着地面高度走，地面隐藏时仍然显示。
 *
 * ⚠️ 全景纹理只能走 `fetchMediaBlob` → `createImageBitmap`（DRD-40）：受保护地址交给 TextureLoader 会 401，
 *    跨域图会污染 canvas，之后截图时 `toBlob` 抛 SecurityError。
 * ⚠️ 宽度超过 4096 的先缩（DR-N-05）：8K 全景解码后 128MB，再上传一份到显存，低端显卡直接丢上下文。
 * ⚠️ 换全景、切回纯色时旧纹理立刻释放（DR-AC-10 看 textures 计数回落），ImageBitmap 一并 close。
 * ⚠️ 球用「几何 scale(−1, 1, 1) + 正面」而不是 `BackSide`：BackSide 从球内看是左右镜像的（three 官方全景示例的做法）。
 * ⚠️ 网格与坐标轴按 1 m 一段画，不用 GridHelper 的整条长线：一条线有一端落在相机背后时，软件渲染（SwiftShader，
 *    显卡被拉黑时 Chrome 就退到它）会把整条线丢掉 —— 近处的网格与坐标轴整片消失，而远处的照常显示。
 */
import {
  BufferGeometry,
  Color,
  Float32BufferAttribute,
  Group,
  LineBasicMaterial,
  LineSegments,
  Mesh,
  MeshBasicMaterial,
  MeshStandardMaterial,
  PlaneGeometry,
  SphereGeometry,
  SRGBColorSpace,
  Texture
} from "three";
import { DIRECTOR_OBJECT_DEFAULTS } from "~/utils/directorCatalog";
import { asObject, bool, DEG, num, str, type EngineContext } from "./context";
import { directorSetLayer, HELPER_CYAN, HELPER_MAGENTA, LAYER_HELPERS } from "./layers";

/** 全景纹理的最大宽度（DR-N-05）。 */
export const DIRECTOR_PANORAMA_MAX_WIDTH = 4096;

const GROUND_SIZE = 400;
const GRID_SIZE = 40;
const GRID_STEP = 1;

/** 网格线（不含过原点的两条，那两条画成坐标轴），每格一段。 */
function gridPoints(size: number, step: number): number[] {
  const half = size / 2;
  const n = Math.round(size / step);
  const out: number[] = [];
  for (let i = 0; i <= n; i++) {
    const k = -half + i * step;
    if (Math.abs(k) < 1e-9) continue;
    for (let j = 0; j < n; j++) {
      const a = -half + j * step;
      const b = a + step;
      out.push(a, 0, k, b, 0, k, k, 0, a, k, 0, b);
    }
  }
  return out;
}

/** 一条过原点的坐标轴（沿 X 或 Z），每格一段。 */
function axisPoints(size: number, step: number, along: "x" | "z"): number[] {
  const half = size / 2;
  const out: number[] = [];
  for (let a = -half; a < half - 1e-9; a += step) {
    if (along === "x") out.push(a, 0, 0, a + step, 0, 0);
    else out.push(0, 0, a, 0, 0, a + step);
  }
  return out;
}

/** 解码全景：按需缩到不超过 `maxWidth`，并在解码时翻转（ImageBitmap 不吃 `texture.flipY`）。 */
export async function directorDecodePanorama(blob: Blob, maxWidth: number): Promise<ImageBitmap> {
  const full = await createImageBitmap(blob, { imageOrientation: "flipY" });
  if (full.width <= maxWidth) return full;
  const width = maxWidth;
  const height = Math.max(1, Math.round((full.height * maxWidth) / full.width));
  try {
    return await createImageBitmap(full, { resizeWidth: width, resizeHeight: height, resizeQuality: "high" });
  } finally {
    full.close();
  }
}

export class Backdrop {
  /** 清屏色（纯色背景，也是全景球之外、黑边之内的底色）。 */
  readonly color = new Color(DIRECTOR_OBJECT_DEFAULTS.scene.background.color);
  readonly ground: Mesh<PlaneGeometry, MeshStandardMaterial>;
  /** 地面高度（stage 局部坐标），贴地吸附用（DRT-13）。 */
  groundHeight = 0;

  private readonly sphere: Mesh<SphereGeometry, MeshBasicMaterial>;
  private readonly helpers = new Group();
  private texture: Texture | null = null;
  private bitmap: ImageBitmap | null = null;
  /** 当前显示或正在加载的全景媒体 ID。 */
  private mediaId: string | null = null;
  private token = 0;
  private disposed = false;
  /** 正在加载的全景（截图前要等它，DRT-17）；没有在加载时为 null。 */
  private loading: Promise<void> | null = null;

  constructor(
    private readonly ctx: EngineContext,
    private readonly fetchMediaBlob: ((mediaId: string) => Promise<Blob>) | null,
    private readonly maxTextureWidth: number
  ) {
    const d = ctx.disposer;
    const sphereGeometry = d.geometry(new SphereGeometry(1, 64, 32));
    sphereGeometry.scale(-1, 1, 1);
    this.sphere = new Mesh(sphereGeometry, d.material(new MeshBasicMaterial({ depthWrite: false, toneMapped: false })));
    this.sphere.name = "panorama";
    this.sphere.renderOrder = -1;
    this.sphere.visible = false;
    ctx.scene.add(this.sphere);

    this.ground = new Mesh(
      d.geometry(new PlaneGeometry(GROUND_SIZE, GROUND_SIZE).rotateX(-Math.PI / 2)),
      d.material(new MeshStandardMaterial({ color: 0x34373d, roughness: 1, metalness: 0 }))
    );
    this.ground.name = "ground";
    this.ground.receiveShadow = true;
    ctx.stage.add(this.ground);

    const segments = (points: number[], material: LineBasicMaterial, lift: number) => {
      const g = d.geometry(new BufferGeometry().setAttribute("position", new Float32BufferAttribute(points, 3)));
      const line = new LineSegments(g, d.material(material));
      line.position.y = lift;
      return line;
    };
    const grid = segments(
      gridPoints(GRID_SIZE, GRID_STEP),
      new LineBasicMaterial({ color: HELPER_CYAN, transparent: true, opacity: 0.28, depthWrite: false }),
      0.001
    );
    // 半透明物体里最先画（DRT-28）：泼溅也是半透明、不写深度，同为 0 时谁先谁后看距离排序，网格后画的话会透过泼溅的地台露出来。
    // 不能反过来把泼溅排到最后：那样机位机身、选中框这些在泼溅前面的半透明辅助会被它盖住
    grid.renderOrder = -0.5;
    // X 轴品红、Z 轴青（辅助专用色，不用红绿蓝：那是变换手柄的颜色，截图检查要能区分）。
    this.helpers.add(
      grid,
      segments(axisPoints(GRID_SIZE, GRID_STEP, "x"), new LineBasicMaterial({ color: HELPER_MAGENTA }), 0.002),
      segments(axisPoints(GRID_SIZE, GRID_STEP, "z"), new LineBasicMaterial({ color: HELPER_CYAN }), 0.002)
    );
    this.helpers.name = "grid";
    directorSetLayer(this.helpers, LAYER_HELPERS);
    ctx.stage.add(this.helpers);

    d.defer(() => {
      this.disposed = true;
      this.token++;
      this.releasePanorama();
    });
  }

  /** 按场景的 `background` 与 `ground` 段更新。 */
  apply(background: unknown, ground: unknown): void {
    const defaults = DIRECTOR_OBJECT_DEFAULTS.scene;
    const b = asObject(background);
    this.color.set(str(b.color, defaults.background.color));
    const mode = str(b.mode, defaults.background.mode);
    const panorama = asObject(b.panorama);
    const mediaId = mode === "panorama" ? str(panorama.mediaId, "") : "";
    if (mediaId) {
      this.sphere.scale.setScalar(Math.max(1, num(panorama.radius, 50)));
      this.sphere.rotation.y = num(panorama.yaw, 0) * DEG;
      this.setPanorama(mediaId);
    } else {
      this.setPanorama(null);
    }

    const g = asObject(ground);
    const height = num(g.height, defaults.ground.height);
    const opacity = Math.min(1, Math.max(0, num(g.opacity, defaults.ground.opacity)));
    this.groundHeight = height;
    this.ground.position.y = height;
    this.ground.visible = bool(g.visible, defaults.ground.visible) && opacity > 0;
    const material = this.ground.material;
    const transparent = opacity < 1;
    if (material.transparent !== transparent) material.needsUpdate = true;
    material.transparent = transparent;
    material.opacity = opacity;
    material.depthWrite = !transparent;
    this.helpers.position.y = height;
    this.ctx.invalidate();
  }

  /** 当前全景纹理的像素尺寸（调试与 DR-N-05 取证用）；没有全景时为 null。 */
  /** 全景纹理加载完（或失败退回纯色）时 resolve：截图前等它，否则截到的是纯色背景（DRT-17 beforeCapture）。 */
  whenIdle(): Promise<void> {
    return this.loading ?? Promise.resolve();
  }

  textureSize(): { width: number; height: number } | null {
    const image = this.texture?.image as { width?: number; height?: number } | undefined;
    return image && image.width && image.height ? { width: image.width, height: image.height } : null;
  }

  private setPanorama(mediaId: string | null): void {
    if (mediaId === this.mediaId) return;
    this.mediaId = mediaId;
    const token = ++this.token;
    if (!mediaId) {
      this.releasePanorama();
      this.ctx.invalidate();
      return;
    }
    const loading = this.load(mediaId, token).finally(() => {
      if (this.loading === loading) this.loading = null;
    });
    this.loading = loading;
  }

  private async load(mediaId: string, token: number): Promise<void> {
    if (!this.fetchMediaBlob) return;
    try {
      const blob = await this.fetchMediaBlob(mediaId);
      if (token !== this.token) return;
      const bitmap = await directorDecodePanorama(blob, this.maxTextureWidth);
      if (token !== this.token || this.disposed) {
        bitmap.close();
        return;
      }
      this.releasePanorama();
      const texture = this.ctx.disposer.texture(new Texture(bitmap));
      texture.colorSpace = SRGBColorSpace;
      texture.flipY = false;
      texture.needsUpdate = true;
      this.texture = texture;
      this.bitmap = bitmap;
      this.sphere.material.map = texture;
      this.sphere.material.needsUpdate = true;
      this.sphere.visible = true;
      this.ctx.invalidate();
    } catch (err) {
      // 取不到就保持纯色：全景来源断开的提示由编辑器负责（DR-AC-10，dev/03 §6.6）。
      if (token === this.token) console.warn(`[director] 全景图 ${mediaId} 加载失败，暂用纯色背景`, err);
    }
  }

  private releasePanorama(): void {
    this.sphere.visible = false;
    this.sphere.material.map = null;
    this.sphere.material.needsUpdate = true;
    this.ctx.disposer.releaseTexture(this.texture);
    this.texture = null;
    this.bitmap?.close();
    this.bitmap = null;
  }
}
