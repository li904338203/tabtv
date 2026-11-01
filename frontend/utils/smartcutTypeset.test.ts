import { describe, expect, it } from "vitest";
import { smartcutParseTypesetBox, smartcutParseTypesetMissing, smartcutTypesetFallbackStyle, smartcutTypesetKey } from "./smartcutTypeset";

describe("smartcutTypeset", () => {
  it("smartcutTypesetKey：样式键的先后不影响键；字幕不带位置框；画面尺寸进键", () => {
    const a = smartcutTypesetKey({ kind: "text", text: "标题", style: { color: "#fff", fontSizePct: 8 }, box: { xPct: 50, yPct: 20 } }, 1920, 1080);
    const b = smartcutTypesetKey({ kind: "text", text: "标题", style: { fontSizePct: 8, color: "#fff" }, box: { yPct: 20, xPct: 50 } }, 1920, 1080);
    expect(a).toBe(b);
    expect(smartcutTypesetKey({ kind: "text", text: "标题" }, 1080, 1920)).not.toBe(smartcutTypesetKey({ kind: "text", text: "标题" }, 1920, 1080));
    expect(smartcutTypesetKey({ kind: "subtitle", text: "字幕", box: { yPct: 1 } }, 1920, 1080)).toBe(smartcutTypesetKey({ kind: "subtitle", text: "字幕" }, 1920, 1080));
  });

  it("smartcutParseTypesetBox / smartcutParseTypesetMissing", () => {
    expect(smartcutParseTypesetBox("10, 20, 300, 40")).toEqual({ x: 10, y: 20, w: 300, h: 40 });
    expect(smartcutParseTypesetBox("10,20,0,40")).toBeNull();
    expect(smartcutParseTypesetBox("nope")).toBeNull();
    expect(smartcutParseTypesetMissing(encodeURIComponent("𠀀𠀀囧"))).toEqual(["𠀀", "囧"]);
    expect(smartcutParseTypesetMissing("%E0%A4%A")).toEqual([]);
  });

  it("smartcutTypesetFallbackStyle：缺省与 Go 一致（字幕 4.5% 贴底 8%、白字黑描边；文字层 8% 正中）", () => {
    const sub = smartcutTypesetFallbackStyle({ kind: "subtitle", text: "x" }, 1000);
    expect(sub).toMatchObject({ fontSize: "45px", bottom: "8%", left: "50%", color: "#ffffff" });
    expect(sub.webkitTextStroke).toBe("3px #000000");
    const text = smartcutTypesetFallbackStyle({ kind: "text", text: "x", box: { xPct: 30, yPct: 20, anchor: "top" } }, 1000);
    expect(text).toMatchObject({ fontSize: "80px", left: "30%", top: "20%", transform: "translate(-50%, 0)" });
    expect(text.webkitTextStroke).toBeUndefined();
  });
});
