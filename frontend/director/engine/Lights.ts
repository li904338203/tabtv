/**
 * 主光、补光与环境光（DR-F-63，dev/03 §4.2 Lights）。
 *
 * 两盏方向光的方向由 `directorLightPosition(水平角, 俯仰角)` 给出（方位 0 = +Z，即默认机位所在一侧、角色正面），
 * 加一盏弱环境光，保证背光面不是死黑。只有主光投影（2048 阴影贴图），阴影相机按场景包围球取范围。
 * 灯光在世界坐标里，不随场景整体变换：整体变换是为了对齐背景，光照方向通常跟着全景里的太阳走。
 *
 * ⚠️ 阴影贴图是 RenderTarget，计入 `renderer.info.memory.textures`：关闭时 `light.dispose()` 释放它，
 *    漏了的话 DR-AC-02 的纹理计数永远剩 1。
 */
import { AmbientLight, DirectionalLight, type Sphere } from "three";
import { DIRECTOR_OBJECT_DEFAULTS } from "~/utils/directorCatalog";
import { directorLightPosition } from "~/utils/directorCamera";
import { asObject, bool, num, str, type EngineContext } from "./context";

type LightDefaults = { azimuth: number; color: string; elevation: number; intensity: number; visible: boolean };

const MIN_SHADOW_RADIUS = 6;

export class Lights {
  readonly key = new DirectionalLight(0xffffff, 1.2);
  readonly fill = new DirectionalLight(0xffffff, 0.5);
  readonly ambient = new AmbientLight(0xffffff, 0.3);
  private readonly angles = { key: { az: 45, el: 50 }, fill: { az: 225, el: 20 } };
  private radius = MIN_SHADOW_RADIUS;
  private centerX = 0;
  private centerY = 0;
  private centerZ = 0;

  constructor(private readonly ctx: EngineContext) {
    this.key.name = "key-light";
    this.fill.name = "fill-light";
    this.key.castShadow = true;
    this.key.shadow.mapSize.set(2048, 2048);
    this.key.shadow.bias = -0.0004;
    this.key.shadow.normalBias = 0.02;
    ctx.scene.add(this.key, this.key.target, this.fill, this.fill.target, this.ambient);
    ctx.disposer.defer(() => {
      this.key.dispose();
      this.fill.dispose();
      this.ambient.dispose();
    });
    this.place();
  }

  /** 按场景的 `lights` 段更新。 */
  apply(lights: unknown): void {
    const l = asObject(lights);
    const defaults = DIRECTOR_OBJECT_DEFAULTS.scene.lights;
    this.applyOne(this.key, asObject(l.key), defaults.key, this.angles.key);
    this.applyOne(this.fill, asObject(l.fill), defaults.fill, this.angles.fill);
    this.place();
    this.ctx.invalidate();
  }

  /** 按内容的包围球调整灯光位置与阴影相机范围（场景很大时阴影不被裁掉，很小时阴影不糊）。 */
  fit(bounds: Sphere): void {
    this.radius = Math.max(MIN_SHADOW_RADIUS, bounds.radius * 1.2);
    this.centerX = bounds.center.x;
    this.centerY = bounds.center.y;
    this.centerZ = bounds.center.z;
    this.place();
  }

  private applyOne(light: DirectionalLight, src: Record<string, unknown>, d: LightDefaults, angle: { az: number; el: number }) {
    light.visible = bool(src.visible, d.visible);
    light.intensity = Math.max(0, num(src.intensity, d.intensity));
    light.color.set(str(src.color, d.color));
    angle.az = num(src.azimuth, d.azimuth);
    angle.el = num(src.elevation, d.elevation);
  }

  private place(): void {
    const distance = this.radius * 2.5;
    for (const [light, angle] of [
      [this.key, this.angles.key],
      [this.fill, this.angles.fill]
    ] as const) {
      const p = directorLightPosition(angle.az, angle.el, distance);
      light.position.set(this.centerX + p.x, this.centerY + p.y, this.centerZ + p.z);
      light.target.position.set(this.centerX, this.centerY, this.centerZ);
      light.target.updateMatrixWorld();
    }
    const cam = this.key.shadow.camera;
    cam.left = -this.radius;
    cam.right = this.radius;
    cam.top = this.radius;
    cam.bottom = -this.radius;
    cam.near = 0.1;
    cam.far = distance + this.radius * 2;
    cam.updateProjectionMatrix();
  }
}
