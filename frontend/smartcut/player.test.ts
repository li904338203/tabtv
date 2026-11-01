import { describe, expect, it, vi } from "vitest";
import { smartcutClaimPlayer } from "./player";

describe("smartcutClaimPlayer", () => {
  it("smartcutClaimPlayer：后开始的让前一个停下；释放之后不再被停", () => {
    const a = vi.fn();
    const b = vi.fn();
    const c = vi.fn();
    const releaseA = smartcutClaimPlayer(a);
    smartcutClaimPlayer(b);
    expect(a).toHaveBeenCalledTimes(1);
    // A 已经被顶掉：它的释放不影响 B
    releaseA();
    smartcutClaimPlayer(c);
    expect(b).toHaveBeenCalledTimes(1);
    expect(a).toHaveBeenCalledTimes(1);
  });
});
