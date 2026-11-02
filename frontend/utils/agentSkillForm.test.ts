import { describe, expect, it } from "vitest";
import {
  skillBindingIssue, skillBodyIssue, skillCoverNote, skillImportRefs, skillModelOptionsFor, moveSkillStep, renumberSkillSteps, skillServerIssues, skillDraft,
  skillExportItem, skillImportPayload, skillPayload, slotsFromText, slotsToText, skillToolMeaning, skillUnknownTools,
  validateSkillDraft, skillWarningsOf, skillWhitelistNotes, type ToolOption
} from "./agentSkillForm";

const tools: ToolOption[] = [
  { name: "load_skill", paid: false, canvasWrite: false, action: "", planning: true, executing: true },
  { name: "generate", paid: true, canvasWrite: true, action: "", planning: false, executing: true }
];
const row = {
  id: "1", code: "story", name: "故事", category: "创作", description: "说明", tags: ["a"], body: "写{{topic}}",
  steps: [{ name: "一", checkpoint: true }, { name: "二" }], tools: ["load_skill"], modelBindings: { video: "v1" },
  isQuickEntry: true, sort: 20, isEnabled: true, version: 3, scenes: ["marketing"],
  inputSlots: [{ key: "product", label: "商品", accept: ["product"], required: true, maxCount: 1 }],
  prefillText: "预填", isBatch: true, isBuiltin: true, isUserModified: false, coverMediaId: "9"
};

describe("skillDraft / skillPayload 往返", () => {
  it("场景与输入四项原样回传", () => {
    const d = skillDraft(row);
    expect(d.steps.map(s => s.no)).toEqual([1, 2]);
    const p = skillPayload(d, "9", "update");
    expect(p.version).toBe(3);
    expect(p.code).toBeUndefined();
    expect(p.inputSlots).toEqual(row.inputSlots);
    expect(p.scenes).toEqual(["marketing"]);
    expect(p.prefillText).toBe("预填");
    expect(p.isBatch).toBe(true);
    expect(p.modelBindings).toEqual({ video: "v1", image: "", reasoning: "" });
  });
  it("新建带 code，不带 version；空封面传 null", () => {
    const p = skillPayload(skillDraft({ ...row, code: " new_one " }), "", "create");
    expect(p.code).toBe("new_one");
    expect(p.version).toBeUndefined();
    expect(p.coverMediaId).toBeNull();
  });
  it("新建表单的默认值", () => {
    const d = skillDraft(null, 30);
    expect(d.sort).toBe(30);
    expect(d.inputSlotsText).toBe("");
    expect(d.isEnabled).toBe(false);
  });
});

describe("输入槽 JSON", () => {
  it("空串 = 不声明；非数组 / 非对象报错", () => {
    expect(slotsFromText("  ")).toEqual({ slots: [], error: "" });
    expect(slotsFromText("{").error).toContain("不是合法的 JSON");
    expect(slotsFromText("{}").error).toContain("JSON 数组");
    expect(slotsFromText("[1]").error).toContain("第 1 项");
    expect(slotsFromText(slotsToText(row.inputSlots)).slots).toEqual(row.inputSlots);
    expect(slotsToText([])).toBe("");
  });
});

describe("步骤", () => {
  it("移动后重编号，越界不动", () => {
    const s = renumberSkillSteps([{ no: 0, name: "a", checkpoint: false }, { no: 0, name: "b", checkpoint: false }]);
    expect(moveSkillStep(s, 0, 1).map(x => `${x.no}${x.name}`)).toEqual(["1b", "2a"]);
    expect(moveSkillStep(s, 0, -1)).toBe(s);
  });
});

describe("校验", () => {
  const ctx = { creating: true, vars: ["topic"], tools, list: [{ code: "story" }] };
  it("逐条给出第一处问题", () => {
    const base = skillDraft({ ...row, code: "fresh" });
    expect(validateSkillDraft(base, ctx)).toBe("");
    expect(validateSkillDraft({ ...base, code: "Bad" }, ctx)).toContain("小写字母开头");
    expect(validateSkillDraft({ ...base, code: "story" }, ctx)).toContain("已存在");
    expect(validateSkillDraft({ ...base, name: " " }, ctx)).toBe("名称不能为空");
    expect(validateSkillDraft({ ...base, tags: ["a", "a"] }, ctx)).toBe("标签有重复");
    expect(validateSkillDraft({ ...base, body: "{{nope}}" }, ctx)).toContain("不存在的变量：nope");
    expect(validateSkillDraft({ ...base, steps: [{ no: 1, name: "", checkpoint: false }] }, ctx)).toBe("第 1 步缺少名称");
    expect(validateSkillDraft({ ...base, tools: ["ghost"] }, ctx)).toContain("ghost");
    expect(validateSkillDraft({ ...base, prefillText: "字".repeat(201) }, ctx)).toContain("最多 200 字");
    expect(validateSkillDraft({ ...base, inputSlotsText: "[" }, ctx)).toContain("输入槽");
    expect(validateSkillDraft({ ...base, code: "story" }, { ...ctx, creating: false })).toBe("");
  });
  it("正文问题", () => {
    expect(skillBodyIssue("主说明", "", [])).toBe("主说明不能为空");
    expect(skillBodyIssue("主说明", "{{a", [])).toContain("未闭合");
  });
});

describe("工具与模型", () => {
  it("工具说明与白名单提醒", () => {
    expect(skillToolMeaning(tools[1]!)).toBe("仅执行态可用（用户确认规划之后） · 会产生费用、会改画布");
    expect(skillUnknownTools(["x", "load_skill"], tools)).toEqual(["x"]);
    expect(skillWhitelistNotes([], 0)[0]).toContain("不收窄");
    expect(skillWhitelistNotes(["generate"], 2).join()).toContain("read_skill_ref");
  });
  it("推理只列支持 Agent 的文本模型", () => {
    const models = [
      { code: "t", name: "T", domain: "text", status: "enabled", agentCapable: true },
      { code: "t2", name: "T2", domain: "text", status: "disabled", agentCapable: false },
      { code: "v", name: "V", domain: "video", status: "disabled", agentCapable: false }
    ];
    expect(skillModelOptionsFor(models, "reasoning").map(o => o.value)).toEqual(["", "t"]);
    expect(skillModelOptionsFor(models, "video")[1]?.label).toContain("已停用");
    expect(skillBindingIssue(models, "reasoning", "t2")).toContain("不支持 Agent");
    expect(skillBindingIssue(models, "image", "v")).toContain("不是图片模型");
    expect(skillBindingIssue(models, "video", "gone")).toContain("不存在");
    expect(skillBindingIssue(models, "video", "")).toBe("");
  });
  it("营销技能没封面要提醒", () => {
    expect(skillCoverNote(["marketing"], false)).toContain("封面");
    expect(skillCoverNote(["marketing"], true)).toBe("");
    expect(skillCoverNote([], false)).toBe("");
  });
});

describe("服务端问题与提示", () => {
  it("issues 与 problems 都收", () => {
    expect(skillServerIssues({ issues: [{ field: "name", message: "空" }], problems: ["正文渲染失败"] })).toEqual(["name：空", "正文渲染失败"]);
    expect(skillServerIssues(undefined)).toEqual([]);
    expect(skillWarningsOf({ warnings: [{ message: "缺口播提示句" }] })).toEqual(["缺口播提示句"]);
  });
});

describe("导出 / 导入", () => {
  it("导出只留内容字段；导入覆盖带当前版本与封面", () => {
    const item = skillExportItem(row, [{ id: "5", name: "附", body: "正文", sort: 1 }]);
    expect(item.id).toBeUndefined();
    expect(item.version).toBeUndefined();
    expect(item.coverMediaId).toBeUndefined();
    expect(item.references).toEqual([{ name: "附", body: "正文", sort: 1 }]);
    const upd = skillImportPayload(item, { version: 7, coverMediaId: "9" });
    expect(upd.version).toBe(7);
    expect(upd.coverMediaId).toBe("9");
    expect(upd.inputSlots).toEqual(row.inputSlots);
    const created = skillImportPayload(item, null);
    expect(created.code).toBe("story");
    expect(created.coverMediaId).toBeNull();
    expect(skillImportRefs(item)).toEqual([{ name: "附", body: "正文", sort: 1 }]);
    expect(skillImportRefs({ references: [{ name: " " }] })).toEqual([]);
  });
});
