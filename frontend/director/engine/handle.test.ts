import { describe, expect, it } from "vitest";
import type { DirectorEngineLike } from "./DirectorEngine";
import { createDirectorEngineHandle } from "./handle";

function fakeEngine(log: string[], name: string): DirectorEngineLike {
  return {
    mount: () => log.push(`${name}.mount`),
    invalidate: () => log.push(`${name}.invalidate`),
    dispose: () => log.push(`${name}.dispose`)
  };
}

const canvas = {} as HTMLCanvasElement;

describe("director engine handle", () => {
  it("卸载即释放：attach 挂载、detach 释放，顺序固定", () => {
    const log: string[] = [];
    const handle = createDirectorEngineHandle(() => fakeEngine(log, "e1"));
    handle.attach(canvas);
    handle.detach();
    expect(log).toEqual(["e1.mount", "e1.dispose"]);
    expect(handle.current()).toBeNull();
  });

  it("重复 detach 只释放一次", () => {
    const log: string[] = [];
    const handle = createDirectorEngineHandle(() => fakeEngine(log, "e1"));
    handle.attach(canvas);
    handle.detach();
    handle.detach();
    expect(log.filter((l) => l.endsWith("dispose"))).toHaveLength(1);
  });

  it("再次 attach 先释放旧引擎，同时只有一个活的渲染器", () => {
    const log: string[] = [];
    let n = 0;
    const handle = createDirectorEngineHandle(() => fakeEngine(log, `e${++n}`));
    handle.attach(canvas);
    handle.attach(canvas);
    expect(log).toEqual(["e1.mount", "e1.dispose", "e2.mount"]);
  });
});
