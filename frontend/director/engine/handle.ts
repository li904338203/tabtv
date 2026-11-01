/**
 * 引擎的生命周期句柄：组件挂载时 attach、卸载时 detach。
 *
 * 单独成一个模块，是为了让「卸载即释放」能用替身引擎做单测（DRT-09）：组件本身在 node 环境的 Vitest 里
 * 加载不了（没有 .vue 插件、没有 WebGL），而释放顺序正是最容易出错、出错后又最难发现的地方 ——
 * 漏掉 dispose 不报错，只是 WebGL 上下文越积越多，直到浏览器开始丢别的上下文。
 */
import type { DirectorEngineDeps, DirectorEngineLike } from "./DirectorEngine";

export interface DirectorEngineHandle<E extends DirectorEngineLike = DirectorEngineLike> {
  /** 在 canvas 上挂一个新引擎；已挂着的话先释放旧的。标签层与依赖原样交给 `mount`（DRT-12）。 */
  attach(canvas: HTMLCanvasElement, labels?: HTMLElement | null, deps?: DirectorEngineDeps): E;
  /** 释放当前引擎；重复调用无副作用。 */
  detach(): void;
  current(): E | null;
}

export function createDirectorEngineHandle<E extends DirectorEngineLike>(factory: () => E): DirectorEngineHandle<E> {
  let engine: E | null = null;
  return {
    attach(canvas, labels, deps) {
      if (engine) engine.dispose();
      engine = factory();
      engine.mount(canvas, labels, deps);
      return engine;
    },
    detach() {
      const current = engine;
      engine = null;
      current?.dispose();
    },
    current() {
      return engine;
    }
  };
}
