import { describe, expect, it } from "vitest";
import {
  smartcutFormatTimecode,
  smartcutFrameMs,
  smartcutFramesToMs,
  smartcutMsToFrames,
  smartcutParseTimecode,
  smartcutQuantizeMs,
  smartcutStepMs,
  smartcutTimecodeFpsOptions
} from "./smartcutTimecode";

describe("帧级时码（SC-F-23）", () => {
  it("边界帧进位：24fps 的第 23 / 24 帧、30fps 的第 29 / 30 帧、25fps 的第 24 / 25 帧", () => {
    expect(smartcutFormatTimecode(smartcutFramesToMs(23, 24), 24)).toBe("00:00:00:23");
    expect(smartcutFormatTimecode(smartcutFramesToMs(24, 24), 24)).toBe("00:00:01:00");
    expect(smartcutFormatTimecode(smartcutFramesToMs(29, 30), 30)).toBe("00:00:00:29");
    expect(smartcutFormatTimecode(smartcutFramesToMs(30, 30), 30)).toBe("00:00:01:00");
    expect(smartcutFormatTimecode(smartcutFramesToMs(24, 25), 25)).toBe("00:00:00:24");
    expect(smartcutFormatTimecode(smartcutFramesToMs(25, 25), 25)).toBe("00:00:01:00");
  });

  // 显示的是播放头所在的那一帧：999ms 在 30fps 下还在第 29 帧里，不提前进位到下一秒。
  it("帧内任意时刻显示所在的那一帧", () => {
    expect(smartcutFormatTimecode(999, 30)).toBe("00:00:00:29");
    expect(smartcutFormatTimecode(1000, 30)).toBe("00:00:01:00");
    expect(smartcutFormatTimecode(41, 24)).toBe("00:00:00:00");
    expect(smartcutFormatTimecode(42, 24)).toBe("00:00:00:01");
  });

  it("分、时进位", () => {
    expect(smartcutFormatTimecode(59_999, 30)).toBe("00:00:59:29");
    expect(smartcutFormatTimecode(60_000, 30)).toBe("00:01:00:00");
    expect(smartcutFormatTimecode(3_600_000 + 61_500, 25)).toBe("01:01:01:12");
  });

  it("负数、NaN 按 0；超长不截断", () => {
    expect(smartcutFormatTimecode(-500, 30)).toBe("00:00:00:00");
    expect(smartcutFormatTimecode(Number.NaN, 30)).toBe("00:00:00:00");
    expect(smartcutFormatTimecode(100 * 3_600_000, 30)).toBe("100:00:00:00");
    expect(smartcutMsToFrames(-1, 24)).toBe(0);
    expect(smartcutFramesToMs(-3, 24)).toBe(0);
  });

  it("帧率非法时按 30，高帧率的帧位是 3 位", () => {
    expect(smartcutFormatTimecode(1000, 0)).toBe("00:00:01:00");
    expect(smartcutFormatTimecode(smartcutFramesToMs(119, 120), 120)).toBe("00:00:00:119");
  });
});

describe("往返一致（24 / 25 / 30fps）", () => {
  it("帧 → 毫秒 → 帧：每一帧都回到自己（含 24fps 的非整毫秒边界）", () => {
    for (const fps of smartcutTimecodeFpsOptions) {
      for (let frame = 0; frame <= fps * 125; frame++) {
        expect(smartcutMsToFrames(smartcutFramesToMs(frame, fps), fps)).toBe(frame);
      }
    }
  });

  it("时码 → 毫秒 → 时码：原样回来", () => {
    for (const fps of smartcutTimecodeFpsOptions) {
      for (const tc of ["00:00:00:00", "00:00:00:01", `00:00:00:${fps - 1}`, "00:00:01:00", "00:12:34:05", "01:59:59:00"]) {
        const ms = smartcutParseTimecode(tc, fps);
        expect(ms).not.toBeNull();
        expect(smartcutFormatTimecode(ms!, fps)).toBe(tc);
      }
    }
  });

  it("毫秒 → 时码 → 毫秒：回到所在帧的起点（= 量化），且不越过原值", () => {
    for (const fps of smartcutTimecodeFpsOptions) {
      for (let ms = 0; ms <= 5000; ms += 7) {
        const back = smartcutParseTimecode(smartcutFormatTimecode(ms, fps), fps)!;
        expect(back).toBe(smartcutQuantizeMs(ms, fps));
        expect(back).toBeLessThanOrEqual(ms);
        expect(ms - back).toBeLessThan(smartcutFrameMs(fps));
      }
    }
  });

  it("MM:SS:FF 的短写也认", () => {
    expect(smartcutParseTimecode("01:02:03", 25)).toBe(62_120);
    expect(smartcutParseTimecode(" 00:01:00:00 ", 30)).toBe(60_000);
  });

  it("格式不对、越界时返回 null，不猜", () => {
    expect(smartcutParseTimecode("", 30)).toBeNull();
    expect(smartcutParseTimecode("1:2", 30)).toBeNull();
    expect(smartcutParseTimecode("00:00:00:30", 30)).toBeNull();
    expect(smartcutParseTimecode("00:60:00:00", 30)).toBeNull();
    expect(smartcutParseTimecode("00:00:60:00", 30)).toBeNull();
    expect(smartcutParseTimecode("00:00:0a:00", 30)).toBeNull();
    expect(smartcutParseTimecode("-1:00:00:00", 30)).toBeNull();
  });
});

describe("逐帧与逐秒（SC-UX-03）", () => {
  it("逐帧：24fps 下连续步进不会原地踏步", () => {
    let ms = 0;
    const seen: number[] = [];
    for (let i = 0; i < 5; i++) {
      ms = smartcutStepMs(ms, 24, "frame", 1);
      seen.push(ms);
    }
    expect(seen).toEqual([42, 84, 125, 167, 209]);
    expect(smartcutStepMs(209, 24, "frame", -1)).toBe(167);
  });

  it("逐秒按帧率个帧走，结果在帧边界上；夹在 [0, 总长]", () => {
    expect(smartcutStepMs(42, 24, "second", 1)).toBe(1042);
    expect(smartcutStepMs(500, 30, "second", -1)).toBe(0);
    expect(smartcutStepMs(9_990, 30, "second", 1, 10_000)).toBe(10_000);
    expect(smartcutStepMs(0, 30, "frame", -1)).toBe(0);
  });
});
