import { describe, expect, it } from "vitest";
import {
  MARKETING_CHAR_GEN_KEY,
  MARKETING_CHAR_GEN_TTL_MS,
  MARKETING_CHARACTER_UPLOAD_NOTE,
  MARKETING_VOICE_NOTE,
  marketingAddCharGen,
  marketingCandidatesOf,
  marketingCharacterCloseConfirmText,
  marketingCharacterFormIssue,
  marketingCharacterFormOf,
  marketingCharacterGenGate,
  marketingCharacterGenKey,
  marketingCharacterGenRequest,
  marketingCharacterInput,
  marketingCharacterLimits,
  marketingCharacterNameFailure,
  marketingCharacterNameHint,
  marketingCharacterNameIssue,
  marketingCharacterPickChanged,
  marketingCharacterSlotItems,
  marketingCreationProjectLabel,
  marketingDismissCandidate,
  marketingEmptyCharacterForm,
  marketingGenStatusText,
  marketingPickedFromSlot,
  marketingPresetImageUrl,
  marketingReadCharGen,
  marketingRescopeCharGen,
  marketingSyncCharacter,
  marketingToggleCharacter,
  marketingVisibleCandidates,
  marketingVoiceGate,
  marketingWriteCharGen,
  type MarketingCharacterForm,
  type MarketingPickedCharacter,
  MARKETING_LIBRARY_FALLBACK,
  MARKETING_READ_ONLY_TEXT,
  marketingAddImages,
  marketingCloseConfirmText,
  marketingConflictText,
  marketingEmptyProductForm,
  marketingImageAccept,
  marketingImageAllowed,
  marketingImageGate,
  marketingImageHint,
  marketingIsQuotaError,
  marketingLibraryQuery,
  marketingMoveImage,
  marketingPatchImage,
  marketingPickChanged,
  marketingProductCountText,
  marketingProductDeleteConfirm,
  marketingProductDirty,
  marketingProductEmptyText,
  marketingProductFormIssue,
  marketingProductFormOf,
  marketingProductInput,
  marketingProductLimits,
  marketingProductQuota,
  marketingProductTitle,
  marketingRemoveImage,
  marketingSetMainImage,
  marketingTextCheck,
  marketingTextLength,
  type MarketingImageItem,
  type MarketingProductForm
} from "./marketingLibrary";

const limits = marketingProductLimits({
  maxProductImages: 10,
  quota: { products: 500, characters: 200 },
  textLimits: { productName: 30, sellingPoints: 300, productDescription: 300, characterName: 50, appearance: 500, voiceText: 500 },
  allowedImageMimes: ["image/png", "image/jpeg", "image/webp"]
});

function img(n: number, patch: Partial<MarketingImageItem> = {}): MarketingImageItem {
  return { localId: `l${n}`, mediaId: `${100 + n}`, status: "done", name: `${n}.png`, ...patch };
}

function many(count: number): MarketingImageItem[] {
  return Array.from({ length: count }, (_, i) => img(i + 1));
}

function form(patch: Partial<MarketingProductForm> = {}): MarketingProductForm {
  return { images: [img(1)], name: "保温杯", sellingPoints: "", description: "", ...patch };
}

describe("上限取配置（MKD-34）", () => {
  it("张数、长度、格式、配额都从 /marketing/config 来；缺字段（旧后端）回落兜底", () => {
    expect(limits).toEqual({ maxImages: 10, quota: 500, name: 30, sellingPoints: 300, description: 300, mimes: ["image/png", "image/jpeg", "image/webp"] });
    const changed = marketingProductLimits({ maxProductImages: 6, textLimits: { productName: 12, sellingPoints: 50, productDescription: 80, characterName: 50, appearance: 500, voiceText: 500 } });
    expect(changed).toMatchObject({ maxImages: 6, name: 12, sellingPoints: 50, description: 80 });
    expect(marketingProductLimits(null)).toEqual({
      maxImages: MARKETING_LIBRARY_FALLBACK.maxProductImages,
      quota: 500,
      name: 30,
      sellingPoints: 300,
      description: 300,
      mimes: MARKETING_LIBRARY_FALLBACK.allowedImageMimes
    });
  });

  it("张数上限取配置：10 张可加满、第 11 张入口禁用「最多 10 张」；改成 6 就是 6（MKT-AC-32）", () => {
    expect(marketingImageGate(many(9), limits.maxImages)).toEqual({ ok: true, remaining: 1, reason: "" });
    expect(marketingImageGate(many(10), limits.maxImages)).toEqual({ ok: false, remaining: 0, reason: "最多 10 张" });
    expect(marketingImageGate(many(6), 6).reason).toBe("最多 6 张");
    const out = marketingAddImages(many(8), [img(20), img(21), img(22)], 10);
    expect(out.items).toHaveLength(10);
    expect(out.added.map(item => item.localId)).toEqual(["l20", "l21"]);
    expect(out.notice).toBe("最多 10 张，有 1 张未添加");
    expect(marketingAddImages([], [img(1)], 10).notice).toBe("");
  });

  it("格式说明与预检：「支持 PNG/JPG/WEBP，最多 10 张」，GIF / BMP 挡掉，image/jpg 当 jpeg", () => {
    expect(marketingImageHint(limits)).toBe("支持 PNG/JPG/WEBP，最多 10 张");
    expect(marketingImageAccept(limits.mimes)).toBe("image/png,image/jpeg,image/webp");
    expect(marketingImageAllowed("image/png", limits.mimes)).toBe(true);
    expect(marketingImageAllowed("image/jpg", limits.mimes)).toBe(true);
    expect(marketingImageAllowed("image/gif", limits.mimes)).toBe(false);
    expect(marketingImageAllowed("image/bmp", limits.mimes)).toBe(false);
    expect(marketingImageAllowed("video/mp4", limits.mimes)).toBe(false);
  });
});

describe("图片条：排序与主图", () => {
  it("首张即主图；拖动 / 设为主图后主图随之变化，请求体顺序与界面一致", () => {
    const moved = marketingMoveImage(many(3), 2, 0);
    expect(moved.map(item => item.mediaId)).toEqual(["103", "101", "102"]);
    const main = marketingSetMainImage(many(3), 1);
    expect(main[0]!.mediaId).toBe("102");
    expect(marketingProductInput(form({ images: main })).imageMediaIds).toEqual(["102", "101", "103"]);
    expect(marketingMoveImage(many(2), 0, 9).map(item => item.mediaId)).toEqual(["101", "102"]);
  });

  it("删除后顺延；晚到的上传结果不把删掉的图带回来", () => {
    const left = marketingRemoveImage(many(3), "l1");
    expect(left[0]!.mediaId).toBe("102");
    const gone = marketingRemoveImage([img(1, { status: "uploading", mediaId: undefined })], "l1");
    expect(marketingPatchImage(gone, "l1", { status: "done", mediaId: "9" })).toEqual([]);
  });
});

describe("码点计数", () => {
  it("emoji 按 1 计；计数「n / 上限」；超长点名字段", () => {
    expect(marketingTextLength("😀")).toBe(1);
    expect("😀".length).toBe(2);
    expect(marketingTextLength("新品👍上市")).toBe(5);
    expect(marketingTextCheck("😀".repeat(30), 30, "商品名称")).toEqual({ length: 30, counter: "30 / 30", error: "" });
    expect(marketingTextCheck("😀".repeat(31), 30, "商品名称").error).toBe("商品名称最多 30 字（当前 31 字）");
    expect(marketingTextCheck("", 300, "商品描述").counter).toBe("0 / 300");
  });
});

describe("保存前校验（MKT-F-93、MKT-AC-32 前端部分）", () => {
  it("无图 / 无名称时保存禁用并说明缺什么；只读理由排最前", () => {
    expect(marketingProductFormIssue(form({ images: [] }), limits)).toBe("请上传商品图");
    expect(marketingProductFormIssue(form({ name: "  " }), limits)).toBe("请填写商品名称");
    expect(marketingProductFormIssue(form({ images: [], name: "" }), limits, true)).toBe(MARKETING_READ_ONLY_TEXT);
    expect(marketingProductFormIssue(form(), limits)).toBe("");
  });

  it("上传中 / 失败要先处理；名称 31、卖点 / 描述 301 码点被拦", () => {
    expect(marketingProductFormIssue(form({ images: [img(1), img(2, { status: "uploading", mediaId: undefined })] }), limits)).toBe("有 1 张图片还在上传");
    expect(marketingProductFormIssue(form({ images: [img(1, { status: "failed", mediaId: undefined })] }), limits)).toBe("有 1 张图片上传失败，请重试或移除");
    expect(marketingProductFormIssue(form({ name: "名".repeat(31) }), limits)).toBe("商品名称最多 30 字（当前 31 字）");
    expect(marketingProductFormIssue(form({ sellingPoints: "🔥".repeat(301) }), limits)).toBe("商品卖点最多 300 字（当前 301 字）");
    expect(marketingProductFormIssue(form({ description: "述".repeat(301) }), limits)).toBe("商品描述最多 300 字（当前 301 字）");
    expect(marketingProductFormIssue(form({ sellingPoints: "🔥".repeat(300) }), limits)).toBe("");
  });

  it("请求体：去首尾空白、只带完成的图、顺序即主图顺序", () => {
    expect(marketingProductInput(form({ name: " 保温杯 ", sellingPoints: " 24 小时 ", images: [img(2), img(1)] }))).toEqual({
      name: "保温杯",
      sellingPoints: "24 小时",
      description: "",
      imageMediaIds: ["102", "101"]
    });
  });

  it("详情 → 表单；未保存改动与上传中关闭前要确认（MKT-UX-15）", () => {
    let n = 0;
    const original = marketingProductFormOf({ imageMediaIds: ["101", "102"], name: "杯", sellingPoints: "", description: "" }, () => `r${++n}`);
    expect(original.images.map(item => [item.localId, item.mediaId, item.status])).toEqual([["r1", "101", "done"], ["r2", "102", "done"]]);
    expect(marketingProductDirty(original, original)).toBe(false);
    expect(marketingCloseConfirmText(original, original)).toBe("");
    expect(marketingProductDirty({ ...original, name: "杯子" }, original)).toBe(true);
    expect(marketingProductDirty({ ...original, images: marketingSetMainImage(original.images, 1) }, original)).toBe(true);
    expect(marketingCloseConfirmText({ ...original, description: "x" }, original)).toBe("有未保存的修改，确定放弃修改？");
    const uploading = { ...original, images: [...original.images, img(9, { status: "uploading", mediaId: undefined })] };
    expect(marketingCloseConfirmText(uploading, original)).toBe("图片还在上传，关闭后上传会被取消，确定放弃修改？");
    expect(marketingCloseConfirmText(marketingEmptyProductForm(), marketingEmptyProductForm())).toBe("");
  });
});

describe("列表与文案", () => {
  it("查询参数：空搜索 / 默认排序 / 第一页不带；搜索去首尾空白", () => {
    expect(marketingLibraryQuery({})).toEqual({ limit: "24" });
    expect(marketingLibraryQuery({ q: "  杯 ", sort: "updated" })).toEqual({ q: "杯", limit: "24" });
    expect(marketingLibraryQuery({ q: "a%b", sort: "name", cursor: "eyJ4Ijox", limit: 12 })).toEqual({ q: "a%b", sort: "name", cursor: "eyJ4Ijox", limit: "12" });
    expect(marketingLibraryQuery({ sort: "created" })).toEqual({ sort: "created", limit: "24" });
  });

  it("空态与无结果分开；张数、标题与选择变化", () => {
    expect(marketingProductEmptyText("")).toBe("暂无商品");
    expect(marketingProductEmptyText(" 杯 ")).toBe("没有匹配的商品");
    expect(marketingProductCountText(3)).toBe("共 3 张");
    expect(marketingProductTitle(" 保温杯 ")).toBe("商品：保温杯");
    expect(marketingProductTitle(undefined)).toBe("");
    expect(marketingPickChanged("1", "1")).toBe(false);
    expect(marketingPickChanged("1", "2")).toBe(true);
    expect(marketingPickChanged("1", "")).toBe(true);
  });

  it("删除确认写明会话不受影响（MKT-UX-18）", () => {
    expect(marketingProductDeleteConfirm("保温杯")).toEqual({
      title: "删除商品「保温杯」？",
      message: "删除后从商品库移除；已用它创作的会话不受影响"
    });
  });

  it("配额：加载到底且达上限、或服务端回过 409 quota 才判满（MKT-F-97）", () => {
    expect(marketingProductQuota({ loaded: 500, finished: false, max: 500 }).full).toBe(false);
    expect(marketingProductQuota({ loaded: 500, finished: true, max: 500 })).toEqual({ full: true, reason: "商品数已达上限（500 个），删除不用的商品后再新增" });
    expect(marketingProductQuota({ loaded: 3, finished: true, max: 500, serverFull: true }).full).toBe(true);
    expect(marketingIsQuotaError({ code: 40901, details: { reason: "quota", max: 500 } })).toBe(true);
    expect(marketingIsQuotaError({ code: 40901, details: {} })).toBe(false);
  });

  it("冲突说清是谁改的，按 bySelf 分流，不匹配文案（CD-58）", () => {
    expect(marketingConflictText({ bySelf: true, currentEditorName: "我" })).toBe("你在另一个窗口里改过这个商品，你的输入还在；重新加载会换成最新内容");
    expect(marketingConflictText({ bySelf: false, currentEditorName: "小王" })).toBe("「小王」刚改过这个商品，你的输入还在；重新加载会换成最新内容");
    expect(marketingConflictText({})).toBe("这个商品已被其他人修改，你的输入还在；重新加载会换成最新内容");
  });
});

// ================================================================ 角色库（MKT-T-39）

const FORBIDDEN = ["@", "<", ">", "/", "\\", "|", ":", "*", "?", "(", ")", "（", "）"];
const charLimits = marketingCharacterLimits({
  maxCharacterImages: 9,
  maxCharacterRefs: 2,
  maxCharacterGenCount: 4,
  quota: { products: 500, characters: 200 },
  characterNameForbidden: FORBIDDEN,
  textLimits: { productName: 30, sellingPoints: 300, productDescription: 300, characterName: 50, appearance: 500, voiceText: 500 },
  allowedImageMimes: ["image/png", "image/jpeg", "image/webp"]
});

function charForm(patch: Partial<MarketingCharacterForm> = {}): MarketingCharacterForm {
  return { ...marketingEmptyCharacterForm(), images: [img(1)], name: "柚子君", ...patch };
}

describe("角色上限与名称规则取配置（MKD-34 / MKD-36）", () => {
  it("张数 / 配额 / 槽位上限 / 生成张数 / 长度 / 禁用字符表都来自配置；缺字段回落兜底", () => {
    expect(charLimits).toMatchObject({ maxImages: 9, quota: 200, maxRefs: 2, genMax: 4, name: 50, appearance: 500, voiceText: 500, forbidden: FORBIDDEN });
    expect(marketingCharacterLimits({ characterNameForbidden: ["#"], maxCharacterImages: 3 })).toMatchObject({ maxImages: 3, forbidden: ["#"] });
    expect(marketingCharacterLimits(null)).toMatchObject({ maxImages: 9, quota: 200, maxRefs: 2, genMax: 4, name: 50 });
  });

  it("名称说明按配置字符表拼（与截图 63 同一句）", () => {
    expect(marketingCharacterNameHint(charLimits)).toBe("1-50 字符，不支持 @ < > / \\ | : * ? ( ) （ ）");
  });

  it("逐个禁用字符都拦，并点名是哪个；与服务端同一张表", () => {
    for (const ch of FORBIDDEN) {
      expect(marketingCharacterNameIssue(`小${ch}美`, charLimits)).toBe(`角色名称不能包含 「${ch}」`);
    }
    expect(marketingCharacterNameIssue("a@b@c/d", charLimits)).toBe("角色名称不能包含 「@」「/」");
    // 配置换了字符表，前端跟着换（不另抄一份）
    expect(marketingCharacterNameIssue("小@美", { name: 50, forbidden: ["#"] })).toBe("");
    expect(marketingCharacterNameIssue("小#美", { name: 50, forbidden: ["#"] })).toBe("角色名称不能包含 「#」");
  });

  it("0 字 / 50 字 / 51 字（按码点，emoji 算 1）", () => {
    expect(marketingCharacterNameIssue("   ", charLimits)).toBe("请填写角色名称");
    expect(marketingCharacterNameIssue("😀".repeat(50), charLimits)).toBe("");
    expect(marketingCharacterNameIssue("😀".repeat(51), charLimits)).toBe("角色名称最多 50 字（当前 51 字）");
  });

  it("服务端的重名 409 与 40001 issues 落到名称上；其余不归名称", () => {
    expect(marketingCharacterNameFailure({ message: "名称重复", details: { field: "name", code: "duplicate" } }, " 小美 ")).toBe("「小美」已有同名角色，换个名字");
    expect(marketingCharacterNameFailure({ message: "参数错误", details: { issues: [{ field: "name", code: "forbidden_char", message: "角色名称不能包含 @" }] } }, "小@美")).toBe("角色名称不能包含 @");
    expect(marketingCharacterNameFailure({ message: "参数错误", details: { issues: [{ field: "imageMediaIds", code: "format" }] } }, "小美")).toBe("");
    expect(marketingCharacterNameFailure({ message: "数量已达上限", details: { reason: "quota" } }, "小美")).toBe("");
  });
});

describe("角色表单（MKT-F-105、MKT-AC-36 前端部分）", () => {
  it("保存的禁用理由给最根本的一条：只读 > 缺图 > 上传 > 张数 > 名称 > 超长 > 音频", () => {
    expect(marketingCharacterFormIssue(charForm(), charLimits, true)).toBe("你在该工作区是只读，不能新增、编辑或删除角色");
    expect(marketingCharacterFormIssue(charForm({ images: [], name: "" }), charLimits)).toBe("请添加至少 1 张角色图片");
    expect(marketingCharacterFormIssue(charForm({ images: [img(1, { status: "uploading", mediaId: undefined })] }), charLimits)).toBe("有 1 张图片还在上传");
    expect(marketingCharacterFormIssue(charForm({ images: [img(1, { status: "failed" })] }), charLimits)).toBe("有 1 张图片上传失败，请重试或移除");
    expect(marketingCharacterFormIssue(charForm({ images: many(10) }), charLimits)).toBe("角色图片最多 9 张");
    expect(marketingCharacterFormIssue(charForm({ images: many(9) }), charLimits)).toBe("");
    expect(marketingCharacterFormIssue(charForm({ name: "小(美)" }), charLimits)).toBe("角色名称不能包含 「(」「)」");
    expect(marketingCharacterFormIssue(charForm({ appearance: "字".repeat(501) }), charLimits)).toBe("形象描述最多 500 字（当前 501 字）");
    expect(marketingCharacterFormIssue(charForm({ voiceText: "字".repeat(501) }), charLimits)).toBe("音色描述最多 500 字（当前 501 字）");
    expect(marketingCharacterFormIssue(charForm({ voice: { localId: "v", status: "uploading", name: "a.mp3" } }), charLimits)).toBe("音频还在上传");
  });

  it("音频至多 1 段：已有一段时第二段入口禁用并说明", () => {
    expect(marketingVoiceGate(null)).toEqual({ ok: true, reason: "" });
    expect(marketingVoiceGate({ localId: "v", mediaId: "9", status: "done", name: "a.mp3" })).toEqual({ ok: false, reason: "只能上传 1 段音频，先删除当前这段" });
  });

  it("请求体：只带完成的图、[0] 主图；空画风为 null；无可见范围等任何多余字段", () => {
    const body = marketingCharacterInput(charForm({
      images: [img(1), img(2, { status: "uploading", mediaId: undefined }), img(3)],
      name: " 柚子君 ",
      appearance: " 圆脸 ",
      voiceText: "",
      voice: { localId: "v", mediaId: "77", status: "done", name: "a.mp3" }
    }));
    expect(body).toEqual({ name: "柚子君", imageMediaIds: ["101", "103"], appearance: "圆脸", styleCode: null, voiceText: "", voiceMediaId: "77" });
    expect(Object.keys(body)).not.toContain("visibility");
  });

  it("详情 → 表单往返", () => {
    let n = 0;
    const back = marketingCharacterFormOf({ imageMediaIds: ["1", "2"], name: "小美", appearance: "a", styleCode: "s1", voiceText: "v", voiceMediaId: "9" }, () => `x${++n}`);
    expect(back.images.map(item => item.mediaId)).toEqual(["1", "2"]);
    expect(back.voice?.mediaId).toBe("9");
    expect(marketingCharacterInput(back)).toEqual({ name: "小美", imageMediaIds: ["1", "2"], appearance: "a", styleCode: "s1", voiceText: "v", voiceMediaId: "9" });
  });

  it("关闭确认：生成中说「生成会继续」；有没加入的候选说 24 小时内可添加；无改动不打扰（MKT-UX-15）", () => {
    const base = charForm();
    expect(marketingCharacterCloseConfirmText(base, base, { generating: false, pendingCandidates: 0 })).toBe("");
    expect(marketingCharacterCloseConfirmText(charForm({ name: "改了" }), base, { generating: false, pendingCandidates: 0 })).toBe("有未保存的修改，关闭后会丢失。确定关闭？");
    expect(marketingCharacterCloseConfirmText(base, base, { generating: true, pendingCandidates: 0 })).toBe("生成会继续，结果可在下次打开该角色时添加。确定关闭？");
    expect(marketingCharacterCloseConfirmText(base, base, { generating: false, pendingCandidates: 3 })).toContain("还有 3 张生成结果没有加入，24 小时内再次打开该角色仍可添加");
  });

  it("文案如实（MK-25、MKT-PARAM-25）", () => {
    expect(MARKETING_VOICE_NOTE).toContain("不会发送给生成模型，也不会用于声音克隆");
    expect(MARKETING_CHARACTER_UPLOAD_NOTE).toBe("请上传非真人形象（卡通、拟人、吉祥物、产品拟人等），不得上传他人肖像");
  });
});

describe("角色选择（MKT-F-101、MKT-UX-14）", () => {
  const a: MarketingPickedCharacter = { kind: "marketing_character", id: "1", name: "柚子君", mainMediaId: "11" };
  const b: MarketingPickedCharacter = { kind: "asset", id: "1", name: "唐僧", mainMediaId: null };
  const c: MarketingPickedCharacter = { kind: "marketing_character", id: "3", name: "小美" };

  it("多选到上限；满了说「最多 2 个角色」；再点一次取消；同 id 不同种类是两个角色", () => {
    let r = marketingToggleCharacter([], a, 2);
    r = marketingToggleCharacter(r.selected, b, 2);
    expect(r.selected).toEqual([a, b]);
    const full = marketingToggleCharacter(r.selected, c, 2);
    expect(full).toEqual({ selected: [a, b], notice: "最多 2 个角色" });
    expect(marketingToggleCharacter(r.selected, a, 2).selected).toEqual([b]);
  });

  it("「添加」只在选择有变化时可点（集合与顺序）", () => {
    expect(marketingCharacterPickChanged([a, b], [a, b])).toBe(false);
    expect(marketingCharacterPickChanged([a, b], [b, a])).toBe(true);
    expect(marketingCharacterPickChanged([a], [])).toBe(true);
  });

  it("写进角色槽：库角色 marketing_character、脚本资产 asset；同一个只放一次；往返回弹窗", () => {
    let n = 0;
    const items = marketingCharacterSlotItems([a, b, a], () => `l${++n}`);
    expect(items.map(item => [item.kind, item.id, item.label, item.thumbMediaId])).toEqual([["marketing_character", "1", "柚子君", "11"], ["asset", "1", "唐僧", undefined]]);
    expect(marketingPickedFromSlot(items)).toEqual([a, { ...b, mainMediaId: null }]);
    // 旧形状（上传的人物图 media）不带回弹窗
    expect(marketingPickedFromSlot([{ localId: "x", kind: "media", id: "5", status: "done", label: "旧图" }])).toEqual([]);
  });

  it("编辑 / 删除同步到卡片；不在槽里原样返回", () => {
    const items = marketingCharacterSlotItems([a, b], () => "l");
    const updated = marketingSyncCharacter(items, { kind: "updated", character: { ...a, name: "柚子君 2", mainMediaId: "12" } });
    expect(updated.changed).toBe(true);
    expect(updated.items[0]).toMatchObject({ label: "柚子君 2", thumbMediaId: "12" });
    const removed = marketingSyncCharacter(items, { kind: "deleted", id: "1" });
    expect(removed).toMatchObject({ changed: true, removed: true });
    expect(removed.items.map(item => item.kind)).toEqual(["asset"]); // 同 id 的脚本资产不受影响
    expect(marketingSyncCharacter(items, { kind: "deleted", id: "99" }).changed).toBe(false);
  });

  it("营销角色图片走公开端点（MKT-UX-16）", () => {
    expect(marketingPresetImageUrl("/api/v1", "42", 0, 320)).toBe("/api/v1/marketing/preset-characters/42/images/0?w=320");
  });
});

describe("生成角色（MKT-F-106、MKT-AC-39 前端部分）", () => {
  const input = { modelCode: "gpt-image", description: " 橘色猫咪吉祥物 ", styleCode: "", referenceMediaIds: [] as string[], count: 2 };

  it("请求体：dryRun 与真生成同一份，只差 dryRun；空画风 / 无参考图不带字段；数量夹到上限", () => {
    expect(marketingCharacterGenRequest(input, 4, true)).toEqual({ modelCode: "gpt-image", description: "橘色猫咪吉祥物", count: 2, dryRun: true });
    const real = marketingCharacterGenRequest({ ...input, styleCode: "s3d", referenceMediaIds: ["5", "6"], count: 9 }, 4, false);
    expect(real).toEqual({ modelCode: "gpt-image", description: "橘色猫咪吉祥物", styleCode: "s3d", referenceMediaIds: ["5", "6"], count: 4, dryRun: false });
    expect(marketingCharacterGenRequest({ ...input, count: 0 }, 4, true).count).toBe(1);
  });

  it("报价键：模型 / 数量 / 参考图 / 画风 / 描述任一变了就变（要重报）", () => {
    const key = marketingCharacterGenKey(input, 4);
    expect(marketingCharacterGenKey({ ...input }, 4)).toBe(key);
    expect(marketingCharacterGenKey({ ...input, modelCode: "other" }, 4)).not.toBe(key);
    expect(marketingCharacterGenKey({ ...input, count: 3 }, 4)).not.toBe(key);
    expect(marketingCharacterGenKey({ ...input, referenceMediaIds: ["5"] }, 4)).not.toBe(key);
    expect(marketingCharacterGenKey({ ...input, styleCode: "s" }, 4)).not.toBe(key);
  });

  const gateBase = {
    readOnly: false, generating: false, description: "猫", appearanceLimit: 500, modelCode: "m", uploadingRefs: 0,
    quoting: false, quoteError: "", quote: { blocked: false }
  };

  it("按钮不显示价格；理由按 只读 > 生成中 > 描述 > 模型 > 校验 > 被拒 排", () => {
    expect(marketingCharacterGenGate(gateBase)).toEqual({ disabled: false, reason: "", label: "生成角色" });
    expect(marketingCharacterGenGate({ ...gateBase, readOnly: true }).reason).toBe("你在该工作区是只读，不能生成角色");
    expect(marketingCharacterGenGate({ ...gateBase, description: " " }).reason).toBe("先写下角色的形象描述");
    expect(marketingCharacterGenGate({ ...gateBase, modelCode: "" }).reason).toBe("请选择图片模型");
    expect(marketingCharacterGenGate({ ...gateBase, quoting: true }).label).toBe("生成角色");
    expect(marketingCharacterGenGate({ ...gateBase, quote: { blocked: true, blockedReason: "参考图超过模型上限（最多 4 张）" } }).reason).toBe("参考图超过模型上限（最多 4 张）");
  });

  it("状态文案：轮询耗尽只说「仍在处理」，不说失败、不提退款（GAP-14）", () => {
    const unknown = marketingGenStatusText("unknown");
    expect(unknown).toBe("仍在处理中，稍后再打开该角色查看结果");
    expect(unknown).not.toMatch(/失败|退/);
    expect(marketingGenStatusText("success", 3)).toBe("已生成 3 张，点选加入角色图片");
    expect(marketingGenStatusText("failed")).toBe("这次没有生成成功");
    expect(marketingGenStatusText("failed")).not.toMatch(/积分|退回/);
  });

  it("候选：只认图片产物；去重；已加入的与点过「不要」的不再显示", () => {
    const got = marketingCandidatesOf("g1", [
      { index: 1, type: "image", mediaId: "b" },
      { index: 0, type: "image", mediaId: "a" },
      { index: 2, type: "text" },
      { index: 3, type: "image", mediaId: null }
    ]);
    expect(got).toEqual([{ mediaId: "a", generationId: "g1" }, { mediaId: "b", generationId: "g1" }]);
    const all = [...got, { mediaId: "a", generationId: "g2" }, { mediaId: "c", generationId: "g2" }];
    expect(marketingVisibleCandidates(all, ["b"], ["c"]).map(item => item.mediaId)).toEqual(["a"]);
  });

  it("候选记录：24 小时过期、按角色分组、新建保存后改记到新角色、坏数据当没有", () => {
    const store = new Map<string, string>();
    const storage = { getItem: (k: string) => store.get(k) ?? null, setItem: (k: string, v: string) => void store.set(k, v), removeItem: (k: string) => void store.delete(k) };
    const now = 1_000_000_000;
    let entries = marketingAddCharGen([], "new", "g1", now - MARKETING_CHAR_GEN_TTL_MS - 1);
    entries = marketingAddCharGen(entries, "new", "g2", now);
    entries = marketingAddCharGen(entries, "new", "g2", now); // 同一个只记一次
    entries = marketingDismissCandidate(entries, "g2", "m1");
    marketingWriteCharGen(storage, entries);
    const read = marketingReadCharGen(storage, now);
    expect(read).toEqual([{ scope: "new", generationId: "g2", createdAt: now, dismissed: ["m1"] }]);
    expect(marketingRescopeCharGen(read, "new", "88")[0]!.scope).toBe("88");
    store.set(MARKETING_CHAR_GEN_KEY, "{坏");
    expect(marketingReadCharGen(storage, now)).toEqual([]);
    marketingWriteCharGen(storage, []);
    expect(store.has(MARKETING_CHAR_GEN_KEY)).toBe(false);
  });
});

describe("我的创作里的项目名（MKT-T-35 遗留）", () => {
  it("生成角色的任务（projectId=0）显示「营销角色生成」，不是「项目已删除」", () => {
    expect(marketingCreationProjectLabel({ projectId: "0", projectName: "" }, "（项目已删除）")).toBe("营销角色生成");
    expect(marketingCreationProjectLabel({ projectId: "12", projectName: "" }, "（项目已删除）")).toBe("（项目已删除）");
    expect(marketingCreationProjectLabel({ projectId: "12", projectName: "春季上新" }, "（项目已删除）")).toBe("春季上新");
  });
});
