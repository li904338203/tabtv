import { describe, expect, it } from "vitest";

import {
  budgetCardView,
  buildTimeline,
  credits,
  emptySuggestions,
  etaText,
  extractSuggestions,
  planCardView,
  questionsSubmission,
  READONLY_TOOLS,
  stepOutputs,
  stepTitle,
  suggestionIcon,
  turnFooter,
  unansweredQuestions,
  type AgentMessage,
  type AgentStep,
  type AgentTurn
} from "./agentMessages";

let seq = 0;
function message(partial: Partial<AgentMessage> & Pick<AgentMessage, "role">): AgentMessage {
  seq += 1;
  return {
    id: `m${seq}`,
    seq,
    content: "",
    refs: [],
    toolCalls: null,
    toolCallId: null,
    turnId: "t1",
    cardId: null,
    createdAt: `2026-10-15T07:00:${String(seq).padStart(2, "0")}Z`,
    ...partial
  };
}

function step(partial: Partial<AgentStep> & Pick<AgentStep, "id" | "tool">): AgentStep {
  return {
    turnId: "t1",
    status: "success",
    attempt: 1,
    display: {},
    generationId: null,
    creditsQuoted: 0,
    errorMessage: "",
    startedAt: "2026-10-15T07:00:30Z",
    finishedAt: null,
    ...partial
  };
}

function turn(partial: Partial<AgentTurn> = {}): AgentTurn {
  return {
    id: "t1",
    trigger: "user",
    status: "success",
    modelCode: "chat",
    creditsCost: 4,
    callCount: 2,
    validOutputs: 1,
    errorCode: "",
    errorMessage: "",
    startedAt: "2026-10-15T07:00:00Z",
    finishedAt: "2026-10-15T07:01:00Z",
    createdAt: "2026-10-15T07:00:00Z",
    ...partial
  };
}

describe("步骤行", () => {
  it("相邻只读步骤合并成一行", () => {
    const items = buildTimeline({
      messages: [message({ role: "user", content: "做个视频" })],
      steps: [
        step({ id: "s1", tool: "read_canvas", display: { nodeCount: 6 }, startedAt: "2026-10-15T07:00:31Z" }),
        step({ id: "s2", tool: "read_doc", display: { fileName: "plan.md" }, startedAt: "2026-10-15T07:00:32Z" }),
        step({ id: "s3", tool: "list_assets", display: { count: 3 }, startedAt: "2026-10-15T07:00:33Z" })
      ],
      turns: [turn({ status: "running", finishedAt: null })]
    });
    const merged = items.filter(item => item.kind === "steps");
    expect(merged).toHaveLength(1);
    expect(merged[0]!.kind === "steps" && merged[0]!.steps.map(s => s.id)).toEqual(["s1", "s2", "s3"]);
    expect(items.filter(item => item.kind === "step")).toHaveLength(0);
  });

  it("付费步骤不参与合并，且把两段只读步骤隔开", () => {
    const items = buildTimeline({
      messages: [],
      steps: [
        step({ id: "s1", tool: "read_canvas", startedAt: "2026-10-15T07:00:31Z" }),
        step({ id: "s2", tool: "generate", generationId: "g1", status: "waiting", startedAt: "2026-10-15T07:00:32Z" }),
        step({ id: "s3", tool: "read_doc", startedAt: "2026-10-15T07:00:33Z" })
      ]
    });
    expect(items.map(item => item.kind)).toEqual(["step", "step", "step"]);
  });

  it("失败的只读步骤单列（要能看到原因与重试）", () => {
    const items = buildTimeline({
      messages: [],
      steps: [
        step({ id: "s1", tool: "read_canvas", startedAt: "2026-10-15T07:00:31Z" }),
        step({ id: "s2", tool: "read_doc", status: "failed", errorMessage: "文档不存在", startedAt: "2026-10-15T07:00:32Z" })
      ]
    });
    expect(items.map(item => item.kind)).toEqual(["step", "step"]);
  });

  it("步骤标题按工具类型", () => {
    expect(stepTitle(step({ id: "a", tool: "load_skill", display: { name: "故事选题" } }))).toEqual({ label: "技能学习", detail: "故事选题" });
    expect(stepTitle(step({ id: "a", tool: "read_canvas", display: { nodeCount: 6 } }))).toEqual({ label: "读取画布", detail: "6 个节点" });
    expect(stepTitle(step({ id: "a", tool: "quote", display: { credits: 60, available: true } }))).toEqual({ label: "估算费用", detail: "✦60" });
    expect(stepTitle(step({ id: "a", tool: "write_doc", display: { path: "/plan.md", fileName: "plan.md" } }))).toEqual({ label: "写入文档", detail: "plan.md" });
    expect(stepTitle(step({ id: "a", tool: "write_doc", display: { path: "/.memory.md" } })).label).toBe("更新项目记忆");
    expect(stepTitle(step({ id: "a", tool: "canvas_apply", display: { nodeIds: ["n1", "n2", "n3"], ops: 4 } }))).toEqual({ label: "更新画布", detail: "新增 3 个节点" });
    expect(stepTitle(
      step({ id: "a", tool: "generate", display: { modelCode: "seedance" } }),
      { nodeType: "video", prompt: "写实古风暗调电影感，浓雾笼罩的古树下，悟空回望" }
    )).toEqual({ label: "生成视频", detail: "写实古风暗调电影感，浓雾笼罩的古树下，悟…" });
    expect(stepTitle(step({ id: "a", tool: "read_doc", status: "failed", errorMessage: "文档不存在" }))).toEqual({ label: "读取文档", detail: "文档不存在" });
  });

  it("营销会话的两个只读工具（MKT-T-24）：浏览选项、查看商品图", () => {
    expect(stepTitle(step({ id: "a", tool: "list_marketing_options", display: { kind: "hook", count: 12 } }))).toEqual({ label: "浏览Hook", detail: "12 项" });
    expect(stepTitle(step({ id: "a", tool: "list_marketing_options", display: {} }), { kind: "creative" })).toEqual({ label: "浏览创意", detail: "" });
    expect(stepTitle(step({ id: "a", tool: "list_marketing_options", display: {} }))).toEqual({ label: "浏览营销选项", detail: "" });
    expect(stepTitle(step({ id: "a", tool: "view_media", display: { slot: "product", index: 3 } }))).toEqual({ label: "查看商品图", detail: "第 3 张" });
    expect(READONLY_TOOLS.has("list_marketing_options") && READONLY_TOOLS.has("view_media")).toBe(true);
  });

  it("生成步骤的提示词从同一轮助手消息的工具调用里按顺序配对", () => {
    const items = buildTimeline({
      messages: [message({ role: "assistant", content: "开始生成", toolCalls: [
        { id: "c1", name: "read_canvas", arguments: {} },
        { id: "c2", name: "generate", arguments: { nodeType: "image", prompt: "第一张" } },
        { id: "c3", name: "generate", arguments: { nodeType: "image", prompt: "第二张" } }
      ] })],
      steps: [
        step({ id: "s1", tool: "generate", generationId: "g1", startedAt: "2026-10-15T07:00:40Z" }),
        step({ id: "s2", tool: "generate", generationId: "g2", startedAt: "2026-10-15T07:00:41Z" })
      ]
    });
    const prompts = items.flatMap(item => (item.kind === "step" ? [String(item.args.prompt)] : []));
    expect(prompts).toEqual(["第一张", "第二张"]);
  });
});

describe("时间线", () => {
  it("tool 消息与只有工具调用的助手消息不显示；系统说明显示，唤醒的任务结局不显示", () => {
    const items = buildTimeline({
      messages: [
        message({ role: "user", content: "hi" }),
        message({ role: "assistant", content: "", toolCalls: [{ id: "c", name: "read_canvas", arguments: {} }] }),
        message({ role: "tool", content: "{}", toolCallId: "c" }),
        // design/03 R-8：写给模型的任务结局不显示（成败在步骤行里已经看得到）
        message({ role: "system_event", content: "所等的 1 个任务已全部结束：成功 1、失败 0、取消 0。" }),
        message({ role: "system_note", content: "本轮已达到调用上限（20 次），先停在这里。" })
      ]
    });
    expect(items.map(item => item.kind)).toEqual(["user", "system"]);
    expect(items[1]!.kind === "system" && items[1]!.message.content).toContain("调用上限");
  });

  it("卡片挂在发出它的步骤下", () => {
    const items = buildTimeline({
      messages: [],
      steps: [step({ id: "s1", tool: "submit_plan", status: "waiting" })],
      cards: [{ id: "c1", stepId: "s1", kind: "plan_confirm", status: "pending", payload: {}, submission: null, submittedBy: null, submittedAt: null, createdAt: "2026-10-15T07:00:31Z" }]
    });
    expect(items).toHaveLength(1);
    expect(items[0]!.kind === "step" && items[0]!.card?.id).toBe("c1");
  });

  it("流式文本在正式消息落库后不重复显示", () => {
    const formal = message({ role: "assistant", content: "完整回答" });
    const items = buildTimeline({ messages: [formal], streaming: { [formal.id]: "完整", other: "还在写" } });
    const texts = items.flatMap(item => (item.kind === "assistant" ? [item.text] : []));
    expect(texts).toEqual(["完整回答", "还在写"]);
    expect(items.at(-1)?.kind === "assistant" && items.at(-1)).toMatchObject({ streaming: true });
  });

  it("被停止的轮次最后一条助手文本标「已停止」", () => {
    const items = buildTimeline({
      messages: [message({ role: "assistant", content: "写到一半" })],
      turns: [turn({ status: "cancelled", errorCode: "stopped" })]
    });
    const assistant = items.find(item => item.kind === "assistant");
    expect(assistant?.kind === "assistant" && assistant.stopped).toBe(true);
  });

  it("结束的轮次在末尾出页脚，最后一轮成功时末尾建议变成胶囊", () => {
    const items = buildTimeline({
      messages: [
        message({ role: "user", content: "做个视频" }),
        message({ role: "assistant", content: "已按以下计划生成，请查看效果。\n\n接下来你可以：\n- [满意，结束]\n- [加字幕和配乐]" })
      ],
      turns: [turn()]
    });
    expect(items.map(item => item.kind)).toEqual(["user", "assistant", "suggestions", "footer"]);
    const assistant = items[1]!;
    expect(assistant.kind === "assistant" && assistant.text).toBe("已按以下计划生成，请查看效果。");
    expect(items[2]!.kind === "suggestions" && items[2]!.items).toEqual(["满意，结束", "加字幕和配乐"]);
  });
});

describe("下一步建议", () => {
  it("认列表与方括号两种写法", () => {
    expect(extractSuggestions("好了\n接下来你可以：\n[满意，结束]  [换成水墨风再来一版]").suggestions).toEqual(["满意，结束", "换成水墨风再来一版"]);
    expect(extractSuggestions("好了\n**接下来你可以：**\n1. 调整表演\n2. 加配乐").suggestions).toEqual(["调整表演", "加配乐"]);
  });

  it("后面还有正文时不摘（不是收尾段）", () => {
    const text = "接下来你可以：\n- 改一下\n\n另外，第三镜还在生成。";
    expect(extractSuggestions(text)).toEqual({ body: text, suggestions: [] });
  });
});

describe("页脚", () => {
  it("页脚退回单列不从花费里减", () => {
    const view = turnFooter({ chat: 4, generation: 60, refunded: 20 }, turn());
    expect(view.chat).toBe("对话 ✦4（2 次调用）");
    expect(view.generation).toBe("生成 ✦60");
    expect(view.refunded).toBe("退回 ✦20");
    // 页脚只显示时间与本轮消耗（design/03 R-6）：消耗 = 对话 + 生成，退回单列、不减
    expect(view.spent).toBe("本轮消耗 ✦64（已退回 ✦20）");
  });

  it("页脚显示完整日期时间；没有退回时只写消耗", () => {
    const view = turnFooter({ chat: 0, generation: 0, refunded: 0 }, turn({ finishedAt: "2026-10-25T09:22:38Z" }));
    expect(view.spent).toBe("本轮消耗 ✦0");
    const local = new Date("2026-10-25T09:22:38Z");
    const pad = (n: number) => String(n).padStart(2, "0");
    expect(view.dateTime).toBe(`${local.getFullYear()}/${pad(local.getMonth() + 1)}/${pad(local.getDate())} ${pad(local.getHours())}:${pad(local.getMinutes())}:${pad(local.getSeconds())}`);
  });

  it("账本没取到时不拿报价顶替", () => {
    const view = turnFooter(undefined, turn({ creditsCost: 4 }));
    expect(view.chat).toBe("对话 —（2 次调用）");
    expect(view.generation).toBe("生成 —");
    expect(view.spent).toBe("本轮消耗 —");
  });
});

describe("产出", () => {
  it("多产出全部列出", () => {
    const out = stepOutputs(step({
      id: "s",
      tool: "generate",
      generationId: "g1",
      display: { generationId: "g1", nodeId: "n1", credits: 8, result: { status: "success", outputs: [
        { idx: 0, type: "image", mediaId: "11" },
        { idx: 1, type: "image", mediaId: "12" },
        { idx: 2, type: "image", mediaId: "13" },
        { idx: 3, type: "image", mediaId: "14" }
      ] } }
    }));
    expect(out?.items.map(item => item.mediaId)).toEqual(["11", "12", "13", "14"]);
    expect(out?.state).toBe("done");
    expect(out?.nodeId).toBe("n1");
  });

  it("还在等任务时是 generating；未受理时没有产出卡", () => {
    expect(stepOutputs(step({ id: "s", tool: "generate", status: "waiting", generationId: "g1" }))?.state).toBe("generating");
    expect(stepOutputs(step({ id: "s", tool: "generate", status: "failed", display: { overBudget: true } }))).toBeUndefined();
    expect(stepOutputs(step({ id: "s", tool: "read_canvas" }))).toBeUndefined();
  });

  it("超过预计时间说比预计慢，不说失败", () => {
    const start = Date.parse("2026-10-15T07:00:00Z");
    expect(etaText("2026-10-15T07:00:00Z", 120, start + 10_000)).toBe("生成中 · 预计约 2 分钟");
    const late = etaText("2026-10-15T07:00:00Z", 120, start + 600_000);
    expect(late).toBe("仍在处理，比预计慢");
    expect(late).not.toContain("失败");
  });
});

describe("卡片", () => {
  it("规划确认卡金额来自 payload，按钮写金额", () => {
    const view = planCardView({
      title: "三打白骨精",
      docPath: "/plan.md",
      items: [{ kind: "video", modelCode: "seedance-2", label: "主片", quantity: 15, unit: "second", credits: 60 }],
      total: 60,
      balance: 6400,
      shortfall: 0,
      canApprove: true
    });
    expect(view.items[0]).toMatchObject({ label: "主片", amount: "✦60" });
    expect(view.items[0]!.meta).toBe("视频 · seedance-2 · 15 秒");
    expect(view.total).toBe("✦60");
    expect(view.remaining).toBe("✦6,340");
    expect(view.approveText).toBe("确认并开始 ✦60");
    expect(view.blockedText).toBeUndefined();
  });

  it("余额不足时写差额且不能确认", () => {
    const view = planCardView({ items: [{ kind: "video", label: "主片", credits: 60 }], total: 60, balance: 20, shortfall: 40, canApprove: false });
    expect(view.canApprove).toBe(false);
    expect(view.blockedText).toBe("余额不足，还差 ✦40");
    expect(view.remaining).toBeUndefined();
  });

  it("充值回来后按最新余额重算差额", () => {
    const payload = { items: [{ kind: "video", label: "主片", credits: 60 }], total: 60, balance: 20, shortfall: 40, canApprove: false };
    expect(planCardView(payload, 100)).toMatchObject({ canApprove: true, remaining: "✦40", balance: "✦100" });
    expect(planCardView(payload, 30)).toMatchObject({ canApprove: false, shortfall: "✦30", blockedText: "余额不足，还差 ✦30" });
    expect(planCardView(payload, 20).canApprove).toBe(false);
  });

  it("有不可用项时说明原因，不显示金额", () => {
    const view = planCardView({ items: [{ kind: "video", label: "主片", credits: 0, blockedCode: "model_offline", blockedReason: "模型已下线" }], total: 0, balance: 100, shortfall: 0, canApprove: false });
    expect(view.items[0]).toMatchObject({ amount: "—", blocked: "模型已下线" });
    expect(view.blockedText).toBe("有计划项暂不可用，请调整规划");
  });

  it("追加额度卡", () => {
    const view = budgetCardView({ reason: "第 3 个镜头重试", requested: 20, approved: 60, used: 60, refunded: 20 });
    expect(view.approveText).toBe("追加 ✦20");
    expect(view.usage).toBe("已花 ✦60 / 额度 ✦60（失败已退回 ✦20）");
  });

  it("问答卡全部答完才可提交", () => {
    const qs = [{ id: "q1", type: "single" }, { id: "q2", type: "multi" }, { id: "q3", type: "text" }];
    expect(unansweredQuestions(qs, {})).toEqual(["q1", "q2", "q3"]);
    expect(unansweredQuestions(qs, { q1: { selected: ["a"] }, q2: { selected: [], other: "  " }, q3: { selected: [], text: "答" } })).toEqual(["q2"]);
    const answers = { q1: { selected: ["a"] }, q2: { selected: [], other: "自定义" }, q3: { selected: [], text: " 答 " } };
    expect(unansweredQuestions(qs, answers)).toEqual([]);
    expect(questionsSubmission(qs, answers)).toEqual({ answers: [
      { questionId: "q1", selected: ["a"] },
      { questionId: "q2", selected: [], other: "自定义" },
      { questionId: "q3", text: "答" }
    ] });
  });
});

describe("空状态", () => {
  it("空画布给写故事类建议", () => {
    expect(emptySuggestions([], [])[0]).toContain("写一个故事");
  });

  it("有图片节点给图生视频建议", () => {
    expect(emptySuggestions([{ id: "n1", nodeType: "image" }], []).join("|")).toContain("图片做一条 15 秒视频");
  });

  it("选中节点时针对该节点", () => {
    expect(emptySuggestions([{ id: "n1", nodeType: "image", title: "悟空" }], ["n1"])[0]).toBe("给「悟空」生成 3 个运镜版本");
  });

  it("每一条推荐词都有对应的小图标（2026-11-01）：改了推荐词文案、图标认不出时这里先红", () => {
    const one = (nodeType: string) => emptySuggestions([{ id: "n1", nodeType, title: "镜头" }], ["n1"]);
    const all = [
      ...one("image"), ...one("video"), ...one("text"), ...one("audio"),
      ...emptySuggestions([{ id: "a", nodeType: "image" }, { id: "b", nodeType: "video" }], ["a", "b"]),
      ...emptySuggestions([{ id: "a", nodeType: "image" }], []),
      ...emptySuggestions([{ id: "a", nodeType: "text" }], []),
      ...emptySuggestions([], [])
    ];
    const icons = Object.fromEntries(all.map(text => [text, suggestionIcon(text)]));
    expect(Object.entries(icons).filter(([, icon]) => icon === "spark")).toEqual([]);
    expect(icons["给「镜头」配音乐和字幕"]).toBe("music");
    expect(icons["把「镜头」换一种运镜再来一版"]).toBe("camera");
    expect(icons["帮我写一个故事并分镜"]).toBe("story");
    expect(icons["帮我把画布上的内容整理成分镜"]).toBe("storyboard");
    expect(icons["给我几个短视频创意方向"]).toBe("idea");
    expect(icons["把选中的 2 个节点串成一条视频"]).toBe("link");
  });

  it("推荐词图标只看「」外面的字：节点名里的关键词不算", () => {
    expect(suggestionIcon("用「运镜测试」做一条 15 秒视频")).toBe("video");
    expect(suggestionIcon("说说「音乐」能怎么用")).toBe("help");
    expect(suggestionIcon("随便聊聊")).toBe("spark");
  });
});

it("积分千分位", () => {
  expect(credits(6400)).toBe("✦6,400");
  expect(credits(undefined)).toBe("✦0");
});
