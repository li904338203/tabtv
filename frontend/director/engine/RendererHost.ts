/**
 * 渲染器与按需渲染循环（DRD-34 / 35，DR-N-03 / 04）。
 *
 * - **一个渲染器**：预览小窗与截图都复用它（scissor / 离屏目标），不开第二个 canvas（DRK-15）。
 * - **按需渲染**：`invalidate()` 置脏，下一帧渲染一次；只有存在连续渲染的原因（相机过渡、走位、拖动）时才逐帧跑。
 *   静止时一个 rAF 回调都没有 —— 常驻 60fps 会让风扇一直转（DR-N-03 的检查给 rAF 打桩计数）。
 * - **关闭即交还上下文**：`renderer.dispose()` 之后 `forceContextLoss()`，不等垃圾回收；
 *   浏览器的 WebGL 上下文有上限（约 16 个），超出会丢掉最早的那个。
 */
import { PCFShadowMap, SRGBColorSpace, WebGLRenderer } from "three";

export interface RendererHostHooks {
  /** 每帧渲染之前调用；返回 true 表示还有东西在动（相机阻尼、过渡、走位），要继续下一帧。 */
  beforeRender(dt: number): boolean;
  /** 组装一帧（主视口、预览小窗、轴向小方块）。 */
  render(): void;
  /** 尺寸变化（CSS 像素）。 */
  resize(width: number, height: number): void;
  /** 上下文丢失（不是我们自己释放引起的那一次）。 */
  contextLost(): void;
}

export class RendererHost {
  readonly renderer: WebGLRenderer;
  width = 1;
  height = 1;
  /** 累计渲染帧数（DR-N-03 的检查用：静止时它不该再涨）。 */
  renders = 0;
  contextLost = false;

  private frame = 0;
  private lastTime = 0;
  private readonly reasons = new Set<string>();
  private readonly observer: ResizeObserver;
  private disposing = false;
  private disposed = false;

  constructor(
    readonly canvas: HTMLCanvasElement,
    private readonly hooks: RendererHostHooks
  ) {
    this.renderer = new WebGLRenderer({ canvas, antialias: true, preserveDrawingBuffer: false });
    this.renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
    this.renderer.outputColorSpace = SRGBColorSpace;
    this.renderer.shadowMap.enabled = true;
    // r186 已移除 PCFSoftShadowMap（设了也会被换成 PCFShadowMap，并打一条警告）。
    this.renderer.shadowMap.type = PCFShadowMap;
    // 一帧里要画好几个视口（主视口、预览、轴向小方块），清屏由引擎按视口自己做。
    this.renderer.autoClear = false;
    canvas.addEventListener("webglcontextlost", this.onContextLost);
    this.observer = new ResizeObserver(() => this.measure());
  }

  /** 各模块就绪之后再开始量尺寸、出第一帧（尺寸回调要用到它们）。 */
  start(): void {
    this.observer.observe(this.canvas);
    this.measure(true);
  }

  /**
   * ⚠️ 释放时也会收到一次（`forceContextLoss` 触发，而且是异步的）：那一次只记下「已丢失」，
   *    不通知编辑器 —— 编辑器正在关闭，再弹「图形环境已重置」就是误报。监听器留到这一次之后再移除，
   *    否则 DR-AC-02 的「收到 webglcontextlost」永远等不到。
   */
  private readonly onContextLost = (event: Event) => {
    event.preventDefault();
    this.contextLost = true;
    if (this.disposing) this.canvas.removeEventListener("webglcontextlost", this.onContextLost);
    else this.hooks.contextLost();
  };

  private measure(force = false): void {
    const w = Math.max(1, Math.round(this.canvas.clientWidth));
    const h = Math.max(1, Math.round(this.canvas.clientHeight));
    if (!force && w === this.width && h === this.height) return;
    this.width = w;
    this.height = h;
    this.renderer.setSize(w, h, false);
    this.hooks.resize(w, h);
    this.invalidate();
  }

  /** 画面需要更新：合并到下一帧，同一帧里调多少次只渲染一次。 */
  invalidate(): void {
    this.schedule();
  }

  /** 登记 / 撤销一个连续渲染的原因（拖动、走位、相机过渡……）。撤销最后一个之后循环自然停下。 */
  setReason(name: string, on: boolean): void {
    if (on) this.reasons.add(name);
    else this.reasons.delete(name);
    if (on) this.schedule();
  }

  hasReasons(): boolean {
    return this.reasons.size > 0;
  }

  private schedule(): void {
    if (this.frame || this.disposed || this.contextLost) return;
    this.frame = requestAnimationFrame(this.tick);
  }

  private readonly tick = (time: number) => {
    this.frame = 0;
    if (this.disposed) return;
    // 第一帧没有上一帧的时间，按 60fps 算；帧间隔夹到 0.25 秒，免得长时间停顿后走位一下子飞出去。
    // 不夹得更紧：软件渲染、低端机上一帧本来就可能要一两百毫秒，夹到 0.1 秒的话按住 W 一秒只走出 1.1 m（实测），
    // 走位速度就不再是 1.5 m/s（DR-AC-06）。页面隐藏、窗口失焦时按键本来就会被清空，不会在后台继续走。
    const dt = this.lastTime ? Math.min((time - this.lastTime) / 1000, 0.25) : 1 / 60;
    this.lastTime = time;
    const moving = this.hooks.beforeRender(dt);
    this.draw();
    if (moving || this.reasons.size > 0) this.schedule();
    else this.lastTime = 0;
  };

  /** 立刻同步渲染一帧（截图、调试读像素用）：读像素要在同一个任务里，画完缓冲区就会被交换。 */
  renderNow(): void {
    if (this.disposed || this.contextLost) return;
    this.hooks.beforeRender(0);
    this.draw();
  }

  private draw(): void {
    this.hooks.render();
    this.renders++;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposing = true;
    this.disposed = true;
    if (this.frame) cancelAnimationFrame(this.frame);
    this.frame = 0;
    this.reasons.clear();
    this.observer.disconnect();
    this.renderer.dispose();
    this.renderer.forceContextLoss();
  }
}
