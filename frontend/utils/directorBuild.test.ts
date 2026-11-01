import { describe, expect, it } from "vitest";
import { directorBuildDisabledReason, directorBuildEntryReason, directorBuildErrorText, directorBuildReferences } from "./directorBuild";

const upstream = (sourceNodeId: string, order: number, mediaId: string | null) => ({ sourceNodeId, title: `上游 ${sourceNodeId}`, order, mediaId });
const fail = (code: number | undefined, message = "", details: Record<string, unknown> = {}) =>
  Object.assign(new Error(message), { statusCode: 422, message, data: code === undefined ? undefined : { code, details, traceId: "t" } });
const config = (extra = {}) => ({ available: true, reason: null, userFixable: false, maxImages: 4, maxPromptChars: 2000, concurrency: 1, perHour: 20, usedThisHour: 3, ...extra });

describe("参考图汇总（DR-F-101）", () => {
  it("directorBuildReferences 排除被选为全景的上游", () => {
    const refs = directorBuildReferences({
      incoming: [upstream("pano", 1, "11"), upstream("a", 2, "12")],
      panoramaSourceNodeId: "pano",
      uploads: [],
      maxImages: 4
    });
    expect(refs).toEqual({ mediaIds: ["12"], dropped: 0, pending: [] });
  });

  it("directorBuildReferences 按媒体ID去重", () => {
    const refs = directorBuildReferences({
      // 入边按连线顺序排在上传之前；同一张图既连线又上传只算一次；还没产出的上游说出来
      incoming: [upstream("b", 2, "22"), upstream("a", 1, "21"), upstream("c", 3, null)],
      uploads: [{ id: "22", type: "image" }, { id: "23", type: "image" }, { id: "24", type: "video" }, { id: "x", type: "image" }],
      maxImages: 4
    });
    expect(refs).toEqual({ mediaIds: ["21", "22", "23"], dropped: 0, pending: ["上游 c"] });
  });

  it("directorBuildReferences 超上限丢弃并返回数量", () => {
    const refs = directorBuildReferences({
      incoming: [upstream("a", 1, "1"), upstream("b", 2, "2")],
      uploads: [{ id: "3", type: "image" }, { id: "4", type: "image" }, { id: "5", type: "image" }],
      maxImages: 3
    });
    expect(refs).toEqual({ mediaIds: ["1", "2", "3"], dropped: 2, pending: [] });
    // 模型不收图（上限 0）：一张都不发，全部报丢
    expect(directorBuildReferences({ incoming: [upstream("a", 1, "1")], uploads: [], maxImages: 0 }).dropped).toBe(1);
  });
});

describe("禁用理由", () => {
  const base = { config: config(), role: "editor" as const, prompt: "古代集市，两人对峙", refs: 1, proposalOpen: false };

  it("directorBuildDisabledReason 只读优先于不可用", () => {
    expect(directorBuildDisabledReason({ ...base, role: "viewer", config: config({ available: false, reason: "后台还没有绑定模型" }) }))
      .toBe("需要编辑权限：你在该工作区是只读成员");
    expect(directorBuildDisabledReason({ ...base, config: config({ available: false, reason: "后台还没有绑定模型" }), proposalOpen: true }))
      .toBe("后台还没有绑定模型");
    expect(directorBuildDisabledReason({ ...base, proposalOpen: true, prompt: "" })).toBe("请先应用或放弃当前的 AI 提案");
  });

  it("directorBuildEntryReason：编辑器入口只看权限与可用性，配置没读到时不拦", () => {
    expect(directorBuildEntryReason({ role: "viewer", config: config({ available: false, reason: "后台还没有绑定模型" }) }))
      .toBe("需要编辑权限：你在该工作区是只读成员");
    expect(directorBuildEntryReason({ role: "editor", config: config({ available: false, reason: "后台还没有绑定模型" }) })).toBe("后台还没有绑定模型");
    expect(directorBuildEntryReason({ role: "editor", config: config({ available: false, reason: null }) })).toBe("AI 搭景暂不可用");
    expect(directorBuildEntryReason({ role: "editor", config: null })).toBeNull();
    // 本小时用完不拦入口：展开之后要看得到「已用 N / M 次」
    expect(directorBuildEntryReason({ role: "editor", config: config({ usedThisHour: 20 }) })).toBeNull();
  });

  it("输入问题：空描述、超长、参考图超限、本小时用完；都没问题时可以发送", () => {
    expect(directorBuildDisabledReason({ ...base, prompt: "   " })).toBe("请先描述想要搭建的场景");
    expect(directorBuildDisabledReason({ ...base, config: config({ maxPromptChars: 4 }), prompt: "古代集市对峙" })).toBe("描述不能超过 4 字");
    expect(directorBuildDisabledReason({ ...base, refs: 5 })).toBe("参考图最多 4 张");
    expect(directorBuildDisabledReason({ ...base, config: config({ maxImages: 0 }) })).toBe("当前模型不收参考图，请先移除参考图");
    expect(directorBuildDisabledReason({ ...base, config: config({ usedThisHour: 20 }) })).toBe("本小时已用完 20 次，请稍后再试");
    expect(directorBuildDisabledReason({ ...base, config: null })).toBe("正在读取 AI 搭景的配置");
    // 取失败不是「不可用」：说成读不到（调用方给重试），只读仍排在它前面
    expect(directorBuildDisabledReason({ ...base, config: null, configError: "暂时读不到 AI 搭景的可用性" })).toBe("暂时读不到 AI 搭景的可用性");
    expect(directorBuildDisabledReason({ ...base, role: "viewer", config: null, configError: "暂时读不到" })).toBe("需要编辑权限：你在该工作区是只读成员");
    expect(directorBuildDisabledReason(base)).toBeNull();
  });
});

describe("错误文案（按业务码）", () => {
  it("directorBuildErrorText 按错误码给文案（42901 带秒数）", () => {
    expect(directorBuildErrorText(fail(42901, "x", { retry_after_seconds: 12.3, scope: "hour" }))).toEqual({ text: "请 13 秒后再试", conflict: false, retryAfterSeconds: 13 });
    expect(directorBuildErrorText(fail(42902)).text).toBe("上一次搭景还没结束");
    expect(directorBuildErrorText(fail(42203, "所选模型不支持该操作或输入")).text).toBe("AI 搭景暂不可用：所选模型不支持该操作或输入");
    expect(directorBuildErrorText(fail(42216)).text).toBe("没能理解这段描述，换个说法再试");
    expect(directorBuildErrorText(fail(50201)).text).toBe("模型暂时不可用，请稍后再试");
    expect(directorBuildErrorText(fail(50203)).text).toBe("模型暂时不可用，请稍后再试");
    expect(directorBuildErrorText(fail(50202)).text).toBe("模型响应超时，请稍后再试");
    expect(directorBuildErrorText(fail(40902))).toEqual({ text: "场景刚被修改", conflict: true });
    expect(directorBuildErrorText(fail(40001, "prompt 超过 2000 字")).text).toBe("prompt 超过 2000 字");
  });

  it("directorBuildErrorText 不匹配 message", () => {
    // message 像限流、码却是解析失败：按码走
    expect(directorBuildErrorText(fail(42216, "请求过于频繁")).text).toBe("没能理解这段描述，换个说法再试");
    // 没有业务码（断网）：兜底，不把英文原文给用户看
    expect(directorBuildErrorText(fail(undefined, "FetchError: request failed")).text).toBe("AI 搭景失败，请检查网络后重试");
    expect(directorBuildErrorText(fail(42901, "")).text).toBe("请求过于频繁，请稍后再试");
  });
});
