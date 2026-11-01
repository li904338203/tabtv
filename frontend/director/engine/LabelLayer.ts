/**
 * 角色名称标签（DR-F-44，dev/03 §4.2 LabelLayer）：CSS2DRenderer 把 DOM 摆到 3D 位置上。
 *
 * 标签是 DOM，本来就**不进** WebGL 的画面：截图、控制图、预演视频读的都是渲染器的像素，标签天然不在里面。
 * 标签对象放 layer 1（编辑辅助）：只用 layer 0 渲染的相机即使拿去跑 CSS2D 也会把它们藏起来。
 * 机位视角下标签层摆到画幅框的位置（与渲染视口重合），否则标签会按整块视口投影、对不上人。
 */
import { CSS2DObject, CSS2DRenderer } from "three/addons/renderers/CSS2DRenderer.js";
import type { Camera, Object3D, Scene } from "three";
import type { DirectorRect } from "~/utils/directorCamera";
import type { Disposer } from "./Disposer";
import { LAYER_HELPERS } from "./layers";

export class LabelLayer {
  private readonly renderer: CSS2DRenderer | null;
  private readonly labels = new Set<CSS2DObject>();
  private shown = true;
  private rect: DirectorRect = { x: 0, y: 0, width: 0, height: 0 };

  constructor(
    private readonly element: HTMLElement | null,
    disposer: Disposer
  ) {
    this.renderer = element ? new CSS2DRenderer({ element }) : null;
    if (element) {
      element.style.position = "absolute";
      element.style.pointerEvents = "none";
    }
    disposer.defer(() => {
      for (const label of this.labels) {
        label.removeFromParent();
        label.element.remove();
      }
      this.labels.clear();
      if (element) element.replaceChildren();
    });
  }

  /** 新建一个标签（挂到 `parent` 上，位置由调用方设）。 */
  create(parent: Object3D, text: string): CSS2DObject {
    const el = document.createElement("div");
    el.className = "director-label";
    el.textContent = text;
    Object.assign(el.style, {
      padding: "1px 6px",
      borderRadius: "4px",
      border: "1px solid var(--portal-on-media-line)",
      background: "var(--portal-on-media-bg)",
      color: "var(--portal-on-media)",
      font: "12px/1.5 system-ui, sans-serif",
      whiteSpace: "nowrap",
      pointerEvents: "none",
      userSelect: "none"
    } satisfies Partial<CSSStyleDeclaration>);
    const label = new CSS2DObject(el);
    // 底边中点对准锚点：标签整体在头顶上方。
    label.center.set(0.5, 1);
    label.layers.set(LAYER_HELPERS);
    parent.add(label);
    this.labels.add(label);
    return label;
  }

  setText(label: CSS2DObject, text: string): void {
    if (label.element.textContent !== text) label.element.textContent = text;
  }

  release(label: CSS2DObject): void {
    this.labels.delete(label);
    label.removeFromParent();
    label.element.remove();
  }

  /** `view.showLabels`（DR-F-44）。 */
  setShown(on: boolean): void {
    this.shown = on;
    if (this.element) this.element.style.display = on ? "" : "none";
  }

  /** 渲染一帧。`rect` 是这一帧主画面在视口里的位置（CSS 像素）。 */
  render(scene: Scene, camera: Camera, rect: DirectorRect): void {
    if (!this.renderer || !this.element || !this.shown) return;
    const r = this.rect;
    if (r.x !== rect.x || r.y !== rect.y || r.width !== rect.width || r.height !== rect.height) {
      this.rect = { ...rect };
      this.element.style.left = `${rect.x}px`;
      this.element.style.top = `${rect.y}px`;
      this.renderer.setSize(rect.width, rect.height);
    }
    this.renderer.render(scene, camera);
  }
}
