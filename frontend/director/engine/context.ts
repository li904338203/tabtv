/**
 * 引擎各模块共用的上下文，以及读场景字段的几个小工具。
 *
 * 场景对象是宽松类型（未知字段原样保留，DRT-11），模块读字段时一律带默认值：校验在 store 保存前做，
 * 引擎拿到的偶尔是正在编辑中的数据（例如刚打了一半的数字），不能因为一个字段缺了就整帧崩掉。
 */
import type { Group, Scene } from "three";
import type { DirectorVec3 } from "~/utils/directorScene";
import type { Disposer } from "./Disposer";

export interface EngineContext {
  /** 世界场景：机位、全景、灯光、编辑辅助挂在这里。 */
  readonly scene: Scene;
  /** 场景整体变换（DR-F-60）：角色、道具、群众与地面挂在这里，机位与全景不挂。 */
  readonly stage: Group;
  readonly disposer: Disposer;
  /** 画面需要更新（合并到下一帧）。 */
  invalidate(): void;
}

export type Json = Record<string, unknown>;

export function asObject(v: unknown): Json {
  return typeof v === "object" && v !== null && !Array.isArray(v) ? (v as Json) : {};
}

export function num(v: unknown, fallback: number): number {
  return typeof v === "number" && Number.isFinite(v) ? v : fallback;
}

export function str(v: unknown, fallback: string): string {
  return typeof v === "string" ? v : fallback;
}

export function bool(v: unknown, fallback: boolean): boolean {
  return typeof v === "boolean" ? v : fallback;
}

export function vec3(v: unknown, fallback: DirectorVec3): DirectorVec3 {
  const o = asObject(v);
  return { x: num(o.x, fallback.x), y: num(o.y, fallback.y), z: num(o.z, fallback.z) };
}

export const DEG = Math.PI / 180;

/** 对外的数值统一保留到 1e-6：拖动、换算会留下 0.30000000000000004 这类尾巴，写进场景只是噪声。 */
export function round6(v: number): number {
  return 0 + Math.round(v * 1e6) / 1e6;
}
