/**
 * 控制图（DRT-27，DR-F-133 / 134，dev/03 §12）：深度图与 OpenPose 骨架图。与截图**同机位、同尺寸、同一时刻**。
 *
 * 深度图：
 * - 近白远黑、线性：`gray = 1 − (d − near) / (far − near)`，d 是沿视线的距离（米）；背景与空白是黑色；
 * - ⚠️ 必须线性、不能被色调映射与色彩空间转换改写：用自写的 ShaderMaterial（不含 tonemapping / colorspace 片段）
 *   覆盖全场景，渲染进不转色彩空间的 8 位目标直接读回。否则近远关系还在、数值却不对，下游会猜错（01 §3.7 第 7 条）；
 * - ⚠️ 覆盖材质要支持蒙皮、体型 morph 与实例化：顶点着色器照抄 three 自己的深度材质的片段顺序，
 *   否则摆了姿势的人按绑定姿势出深度、群众整阵缩成一个人；
 * - 只画 layer 0（辅助都在 layer 1，DRD-39）。
 *
 * 骨架图：黑底 2D canvas 上按 OpenPose 标准配色画 17 条连线与 18 个点；点的半径 = 画幅宽 ÷ 320 取整。
 * 被挡住的关节照常画（不做遮挡判断，界面上说明）。
 */
import { Color, NoColorSpace, ShaderMaterial, UnsignedByteType, WebGLRenderTarget, type PerspectiveCamera, type Scene, type WebGLRenderer } from "three";
import { DIRECTOR_OPENPOSE_COLORS, DIRECTOR_OPENPOSE_LIMBS } from "~/utils/directorPose";
import { DirectorCaptureError } from "./Capture";
import { LAYER_CONTENT } from "./layers";

const DEPTH_VERTEX = /* glsl */ `
#include <common>
#include <batching_pars_vertex>
#include <morphtarget_pars_vertex>
#include <skinning_pars_vertex>
varying float vViewDepth;
void main() {
  #include <batching_vertex>
  #include <skinbase_vertex>
  #include <morphinstance_vertex>
  #include <begin_vertex>
  #include <morphtarget_vertex>
  #include <skinning_vertex>
  #include <project_vertex>
  vViewDepth = -mvPosition.z;
}
`;

const DEPTH_FRAGMENT = /* glsl */ `
uniform float uNear;
uniform float uFar;
varying float vViewDepth;
void main() {
  float gray = clamp(1.0 - (vViewDepth - uNear) / max(uFar - uNear, 1e-6), 0.0, 1.0);
  gl_FragColor = vec4(vec3(gray), 1.0);
}
`;

/** 深度覆盖材质（每个引擎一份，由调用方登记进 Disposer）。 */
export function directorDepthMaterial(): ShaderMaterial {
  return new ShaderMaterial({
    name: "director-depth",
    uniforms: { uNear: { value: 0.1 }, uFar: { value: 10 } },
    vertexShader: DEPTH_VERTEX,
    fragmentShader: DEPTH_FRAGMENT,
    toneMapped: false
  });
}

/** 把 RGBA 像素（第 0 行在底部）编码成 PNG。 */
async function encodePixels(pixels: Uint8Array, width: number, height: number): Promise<Blob> {
  const flipped = new Uint8ClampedArray(width * height * 4);
  const row = width * 4;
  for (let y = 0; y < height; y++) flipped.set(pixels.subarray((height - 1 - y) * row, (height - y) * row), y * row);
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext("2d");
  if (!ctx) throw new DirectorCaptureError("encode", "浏览器无法生成控制图");
  ctx.putImageData(new ImageData(flipped, width, height), 0, 0);
  return canvasToPng(canvas);
}

async function canvasToPng(canvas: HTMLCanvasElement): Promise<Blob> {
  const blob = await new Promise<Blob | null>(resolve => canvas.toBlob(resolve, "image/png"));
  canvas.width = 0;
  canvas.height = 0;
  if (!blob) throw new DirectorCaptureError("encode", "浏览器无法生成控制图");
  return blob;
}

/**
 * 离屏渲染一张深度图（`width × height`，与截图同尺寸）。渲染器的目标、清屏色、裁剪、阴影开关与场景的覆盖材质
 * 在 finally 里原样恢复：截完视口照常画。
 */
export async function directorRenderDepth(options: {
  renderer: WebGLRenderer;
  scene: Scene;
  camera: PerspectiveCamera;
  material: ShaderMaterial;
  width: number;
  height: number;
  near: number;
  far: number;
}): Promise<Blob> {
  const { renderer, scene, camera, material, width, height } = options;
  const target = new WebGLRenderTarget(width, height, { samples: 4, type: UnsignedByteType, colorSpace: NoColorSpace });
  const previous = {
    target: renderer.getRenderTarget(),
    clearColor: renderer.getClearColor(new Color()),
    clearAlpha: renderer.getClearAlpha(),
    scissorTest: renderer.getScissorTest(),
    shadowAuto: renderer.shadowMap.autoUpdate,
    override: scene.overrideMaterial,
    layers: camera.layers.mask,
    aspect: camera.aspect
  };
  try {
    material.uniforms.uNear!.value = options.near;
    material.uniforms.uFar!.value = options.far;
    camera.layers.set(LAYER_CONTENT);
    camera.aspect = width / height;
    camera.updateProjectionMatrix();
    scene.updateMatrixWorld();
    scene.overrideMaterial = material;
    renderer.setScissorTest(false);
    renderer.shadowMap.autoUpdate = false;
    renderer.setRenderTarget(target);
    renderer.setClearColor(0x000000, 1);
    renderer.clear();
    renderer.render(scene, camera);
    const pixels = new Uint8Array(width * height * 4);
    renderer.readRenderTargetPixels(target, 0, 0, width, height, pixels);
    return await encodePixels(pixels, width, height);
  } finally {
    scene.overrideMaterial = previous.override;
    camera.layers.mask = previous.layers;
    camera.aspect = previous.aspect;
    camera.updateProjectionMatrix();
    renderer.setRenderTarget(previous.target);
    renderer.setClearColor(previous.clearColor, previous.clearAlpha);
    renderer.setScissorTest(previous.scissorTest);
    renderer.shadowMap.autoUpdate = previous.shadowAuto;
    target.dispose();
  }
}

/** 一个人的 18 个点在截图里的像素坐标（画面外或取不到的为 null）。 */
export type DirectorPose2D = Array<{ x: number; y: number } | null>;

/**
 * 在黑底上画 OpenPose 骨架（每个人一套）。连线宽度与点半径都按画幅宽 ÷ 320 取整（至少 1 像素）。
 * 连线两端有一端缺失就不画这条线。
 */
export async function directorDrawPose(people: readonly DirectorPose2D[], width: number, height: number): Promise<Blob> {
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext("2d");
  if (!ctx) throw new DirectorCaptureError("encode", "浏览器无法生成控制图");
  ctx.fillStyle = "#000000";
  ctx.fillRect(0, 0, width, height);
  const radius = Math.max(1, Math.round(width / 320));
  ctx.lineCap = "round";
  ctx.lineWidth = radius;
  for (const points of people) {
    DIRECTOR_OPENPOSE_LIMBS.forEach(([a, b], index) => {
      const from = points[a];
      const to = points[b];
      if (!from || !to) return;
      ctx.strokeStyle = DIRECTOR_OPENPOSE_COLORS[index] as string;
      ctx.beginPath();
      ctx.moveTo(from.x, from.y);
      ctx.lineTo(to.x, to.y);
      ctx.stroke();
    });
    points.forEach((point, index) => {
      if (!point) return;
      ctx.fillStyle = DIRECTOR_OPENPOSE_COLORS[index] as string;
      ctx.beginPath();
      ctx.arc(point.x, point.y, radius, 0, Math.PI * 2);
      ctx.fill();
    });
  }
  return canvasToPng(canvas);
}
