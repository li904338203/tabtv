/**
 * 截图（DRT-17，DRD-39，dev/03 §7.1）：离屏、固定像素、只画 layer 0。
 *
 * ⚠️ 必须离屏渲染固定像素：截屏幕上的画布的话，尺寸随设备像素比与窗口大小变（01 §3.7 第 3 条），
 *    同一个机位两台电脑截出来不一样大。尺寸只由画幅决定（`directorCaptureSize`，DR-PARAM-08）。
 * ⚠️ 辅助元素靠**图层**排除（手柄、网格、机位模型、选中框都在 layer 1），不靠「截图前逐个隐藏」：
 *    后者迟早漏掉新加的辅助元素（DRD-39）。名称标签是 DOM，本来就不在 WebGL 里。
 * ⚠️ 颜色要与视口一致：three 渲染进普通渲染目标时输出的是**线性**颜色、不做色调映射（只有画布与 XR 目标才转），
 *    直接读回来整张图偏暗。所以先渲染进半浮点的线性目标（4 倍多重采样抗锯齿），再用 OutputPass
 *    按渲染器的输出色彩空间（sRGB）转一次写进 8 位目标，读回来的就是视口里看到的颜色。
 */
import { Color, HalfFloatType, WebGLRenderTarget, type Object3D, type PerspectiveCamera, type Scene, type WebGLRenderer } from "three";
import { OutputPass } from "three/examples/jsm/postprocessing/OutputPass.js";
import { LAYER_CONTENT } from "./layers";

export type DirectorCaptureResult = { blob: Blob; width: number; height: number };

/** 截图失败的原因：界面据此给出路（机位已删除 → 改用输出机位）。 */
export class DirectorCaptureError extends Error {
  constructor(readonly reason: "camera_missing" | "not_mounted" | "encode", message: string) {
    super(message);
    this.name = "DirectorCaptureError";
  }
}

const _clear = new Color();

/**
 * 用 `camera` 离屏渲染一帧 `width × height`，返回**自上而下**的 RGBA 像素（已翻转，可直接放进 ImageData）。
 * 截图（编码 PNG）与预演视频（DRT-25，逐帧交给编码器）共用这一条渲染路径：只画 layer 0、颜色与视口一致。
 * 渲染器的渲染目标、视口、裁剪、清屏色、阴影更新开关在 finally 里原样恢复：截完视口照常画。
 */
export function directorRenderPixels(options: {
  renderer: WebGLRenderer;
  scene: Scene;
  camera: PerspectiveCamera;
  width: number;
  height: number;
  clearColor: Color | number;
  clearAlpha?: number;
}): Uint8ClampedArray<ArrayBuffer> {
  const { renderer, scene, camera, width, height } = options;
  const linear = new WebGLRenderTarget(width, height, { type: HalfFloatType, samples: 4 });
  const output = new WebGLRenderTarget(width, height);
  const pass = new OutputPass();
  const previous = {
    target: renderer.getRenderTarget(),
    clearColor: renderer.getClearColor(new Color()),
    clearAlpha: renderer.getClearAlpha(),
    scissorTest: renderer.getScissorTest(),
    shadowAuto: renderer.shadowMap.autoUpdate,
    layers: camera.layers.mask,
    aspect: camera.aspect
  };
  try {
    camera.layers.set(LAYER_CONTENT);
    camera.aspect = width / height;
    camera.updateProjectionMatrix();
    scene.updateMatrixWorld();
    renderer.setScissorTest(false);
    renderer.shadowMap.autoUpdate = false;
    renderer.shadowMap.needsUpdate = true;
    renderer.setRenderTarget(linear);
    renderer.setClearColor(_clear.set(options.clearColor), options.clearAlpha ?? 1);
    renderer.clear();
    renderer.render(scene, camera);
    pass.render(renderer, output, linear, 0, false);
    const pixels = new Uint8Array(width * height * 4);
    renderer.readRenderTargetPixels(output, 0, 0, width, height, pixels);
    return flipRows(pixels, width, height);
  } finally {
    camera.layers.mask = previous.layers;
    camera.aspect = previous.aspect;
    camera.updateProjectionMatrix();
    renderer.setRenderTarget(previous.target);
    renderer.setClearColor(previous.clearColor, previous.clearAlpha);
    renderer.setScissorTest(previous.scissorTest);
    renderer.shadowMap.autoUpdate = previous.shadowAuto;
    linear.dispose();
    output.dispose();
    pass.dispose();
  }
}

/**
 * 用 `camera` 离屏渲染一张 `width × height` 的图片（默认 PNG）。见 `directorRenderPixels`。
 */
export async function directorRenderCapture(options: {
  renderer: WebGLRenderer;
  scene: Scene;
  camera: PerspectiveCamera;
  width: number;
  height: number;
  clearColor: Color | number;
  /** 背景透明度：截图是 1；姿势缩略图（DRT-21）用 0，界面上按主题的底色显示。 */
  clearAlpha?: number;
  /** 编码格式：截图是 PNG；姿势缩略图是 WebP（体积小一个数量级）。 */
  mime?: "image/png" | "image/webp";
  quality?: number;
}): Promise<DirectorCaptureResult & { objects: string[] }> {
  const pixels = directorRenderPixels(options);
  const blob = await encodeImage(pixels, options.width, options.height, options.mime ?? "image/png", options.quality);
  return { blob, width: options.width, height: options.height, objects: layerZeroObjects(options.scene) };
}

/** 读回的像素第 0 行在底部：上下翻转成自上而下。 */
function flipRows(pixels: Uint8Array, width: number, height: number): Uint8ClampedArray<ArrayBuffer> {
  const flipped = new Uint8ClampedArray(width * height * 4);
  const row = width * 4;
  for (let y = 0; y < height; y++) {
    flipped.set(pixels.subarray((height - 1 - y) * row, (height - y) * row), y * row);
  }
  return flipped;
}

/** 自上而下的 RGBA 像素经 2D canvas 编码（PNG / WebP）。 */
async function encodeImage(pixels: Uint8ClampedArray<ArrayBuffer>, width: number, height: number, mime: string, quality?: number): Promise<Blob> {
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext("2d");
  if (!ctx) throw new DirectorCaptureError("encode", "浏览器无法生成截图");
  ctx.putImageData(new ImageData(pixels, width, height), 0, 0);
  const blob = await new Promise<Blob | null>(resolve => canvas.toBlob(resolve, mime, quality));
  canvas.width = 0;
  canvas.height = 0;
  if (!blob) throw new DirectorCaptureError("encode", "浏览器无法生成截图");
  return blob;
}

/** 这一帧 layer 0 里实际会画的东西（名称或类型）：调试钩子与检查脚本据此断言手柄、网格不在截图里。 */
function layerZeroObjects(scene: Scene): string[] {
  const names: string[] = [];
  scene.traverseVisible((o: Object3D) => {
    const drawable = (o as { isMesh?: boolean; isLine?: boolean; isPoints?: boolean; isSprite?: boolean });
    if (!(drawable.isMesh || drawable.isLine || drawable.isPoints || drawable.isSprite)) return;
    if (o.layers.isEnabled(LAYER_CONTENT)) names.push(o.name || o.type);
  });
  return names;
}
