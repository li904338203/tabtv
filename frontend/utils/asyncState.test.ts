import { describe, expect, it } from "vitest";

import {
  VERSION_CONFLICT,
  canvasSaveState,
  errorCode,
  isTerminalStatus,
  pollExhausted,
  templateApplyState
} from "./asyncState";

/**
 * ⚠️ 这两组判定在移植老前端时**丢过一次测试**：原来的用例长在被删掉的那份前端上
 * （`app/features/canvas/useCanvasSave.test.ts` 与
 * `app/features/generation/useGenerationStatus.test.ts`），
 * 行为本身活了下来、验证没有。`make acceptance` 当场报出证据核不上 ——
 * 那正是它该做的事。
 */

describe("画布保存冲突（AC-03 / FLOW-09 / FE-03）", () => {
  it("40902 进冲突态，其余一律普通失败", () => {
    expect(canvasSaveState({ data: { code: VERSION_CONFLICT } })).toBe("conflict");
    expect(canvasSaveState({ data: { code: 40001 } })).toBe("error");
    expect(canvasSaveState(new Error("Failed to fetch"))).toBe("error");
    expect(canvasSaveState(undefined)).toBe("error");
  });

  it("冲突与失败必须是两个态，不能合并", () => {
    // ⚠️ 合并成一个 error 的话，冲突会被当成普通失败去重试 —— 而重试**必然再次 409**；
    //    反过来把断网当成冲突，用户会被要求在两份内容之间做一个并不存在的选择。
    expect(canvasSaveState({ data: { code: VERSION_CONFLICT } }))
      .not.toBe(canvasSaveState({ data: { code: 50000 } }));
  });

  it("取不到业务码时返回 undefined，而不是 0", () => {
    // 0 是个合法码位；用 0 兜底的话「没有码」与「码是 0」就再也分不开了。
    expect(errorCode({ data: { code: 0 } })).toBe(0);
    expect(errorCode({ data: {} })).toBeUndefined();
    expect(errorCode({ data: { code: "40902" } })).toBeUndefined();
    expect(errorCode("boom")).toBeUndefined();
  });
});

describe("轮询耗尽不等于任务失败（AC-13 / FLOW-04 / FE-04、GAP-14）", () => {
  it("耗尽后写回 unknown，不是 failed", () => {
    expect(pollExhausted().status).toBe("unknown");
    expect(pollExhausted().status).not.toBe("failed");
  });

  it("⚠️ 文案里不能出现「失败」二字", () => {
    // 用户是按文案行动的：写了"失败"，他就会去点重试，而重试是**再扣一次费**。
    expect(pollExhausted().error).not.toMatch(/失败|错误|出错/);
    expect(pollExhausted().error).toMatch(/仍在处理/);
  });

  it("三个终态都要认，少一个就会一直轮询到耗尽", () => {
    expect(isTerminalStatus("success")).toBe(true);
    expect(isTerminalStatus("failed")).toBe(true);
    // ⚠️ `cancelled` 在新后端是**独立终态**（不再是 failed + errorCode）。
    //    少认它的表现不是报错，而是一个已取消的任务轮询 180 次之后
    //    显示"仍在处理中"。
    expect(isTerminalStatus("cancelled")).toBe(true);
    expect(isTerminalStatus("queued")).toBe(false);
    expect(isTerminalStatus("running")).toBe(false);
    expect(isTerminalStatus("unknown")).toBe(false);
    expect(isTerminalStatus(undefined)).toBe(false);
  });
});

describe("templateApplyState", () => {
  const fail = (code: number) => Object.assign(new Error("x"), { data: { code } });
  it("版本冲突与「服务端画布其实不空」都说明本地是旧的 → 冲突态", () => {
    expect(templateApplyState(fail(40902))).toBe("conflict");
    expect(templateApplyState(fail(40905))).toBe("conflict");
  });
  it("其余失败不动画布状态（画布并没有保存失败）", () => {
    expect(templateApplyState(fail(40401))).toBeUndefined();
    expect(templateApplyState(new TypeError("Failed to fetch"))).toBeUndefined();
    expect(templateApplyState(undefined)).toBeUndefined();
  });
});
