package domain

import (
	"strings"
	"testing"
)

// TestOutputContract_与解析器一致：每个模式的契约都带固定标题与模式码，示例按同一份解析器过白名单
// 一处都不丢 —— 契约里教模型写的，解析器一定认（手抄的码表早晚会与解析器不一致，表现是模型照着契约写、被当成不合规丢掉）。
func TestOutputContract_与解析器一致(t *testing.T) {
	cards := []Card{videoCard("1", 8000), videoCard("2", 8000), videoCard("3", 8000), videoCard("5", 8000),
		videoCard("6", 8000), musicCard("4", 20000), musicCard("8", 20000)}
	examples := map[string]struct {
		example string
		refs    Refs
	}{
		ModeMix:       {mixExample, Refs{Cards: cards}},
		ModeBoard:     {boardExample, Refs{Cards: cards, Shots: []string{"b1", "b2", "b3"}}},
		ModeTalk:      {talkExample, Refs{Sentences: []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9"}}},
		ModeNarr:      {narrExample, Refs{Cards: cards, Sentences: []string{"s1", "s2", "s3", "s4"}}},
		ModeEdit:      {editExample, Refs{Cards: cards, Clips: []string{"c1", "c2", "c3", "c4"}}},
		ModeTranslate: {translateExample, Refs{Items: 2}},
	}
	for mode, ex := range examples {
		c := OutputContract(mode)
		if !strings.HasPrefix(c, ContractHeading+"\n模式："+mode+"（") {
			t.Errorf("%s：契约要以固定标题与模式码开头：%.60s", mode, c)
		}
		if strings.Contains(c, "分镜") {
			t.Errorf("%s：契约里不能出现「分镜」（假上游会回分镜 JSON）", mode)
		}
		if !strings.Contains(c, ex.example) {
			t.Errorf("%s：契约里要带示例", mode)
		}
		pr, err := ParseDraft(mode, ex.example, ex.refs)
		if err != nil {
			t.Errorf("%s：示例过不了解析器：%v", mode, err)
			continue
		}
		if len(pr.Dropped) > 0 || len(pr.Ignored) > 0 {
			t.Errorf("%s：示例里有不合规或被忽略的：%v %v", mode, pr.Dropped, pr.Ignored)
		}
	}
	// 码表与解析器一致：枚举在契约里一个不少。
	mix := OutputContract(ModeMix)
	for _, word := range []string{"full", "head", "middle", "tail", "aroundSec", "hook", "body", "climax", "end",
		"cut", "fade", "dissolve", "slide_left", "wipe", "ratio", "weight", "subtitle", "reason", "title", "music", "clipNotes"} {
		if !strings.Contains(mix, word) {
			t.Errorf("素材混剪的契约缺「%s」", word)
		}
	}
	edit := OutputContract(ModeEdit)
	for op := range editOps {
		if !strings.Contains(edit, `"op": "`+op+`"`) {
			t.Errorf("追加指令的契约缺操作 %s", op)
		}
	}
	if OutputContract(ModeMix) != OutputContract(ModeMix) {
		t.Error("同一版本的代码每次返回同一段文字")
	}
}

func TestRepairHint(t *testing.T) {
	if RepairHint(nil, nil) != "" {
		t.Error("没有问题时不回灌")
	}
	h := RepairHint([]string{"第 1 段：编号「9」不在素材条里", "第 2 段：weight 5 不在 1~3 之间"}, nil)
	if !strings.HasPrefix(h, "上一次的输出有以下问题") || !strings.Contains(h, "- 第 1 段：编号「9」不在素材条里\n- 第 2 段：weight 5 不在 1~3 之间") {
		t.Fatalf("%s", h)
	}
	// 不合规的原因已经带着第一处问题，不重复列。
	err := &ErrDraftInvalid{Reason: "模型没有给出可用的片段：第 1 段：编号「9」不在素材条里"}
	h = RepairHint([]string{"第 1 段：编号「9」不在素材条里", "第 2 段：x"}, err)
	if strings.Count(h, "编号「9」不在素材条里") != 1 || !strings.Contains(h, "第 2 段：x") {
		t.Fatalf("%s", h)
	}
}

func TestCardsFromInputs(t *testing.T) {
	d2, d0 := 2000, 0
	yes := true
	items := []Input{
		{No: "1", Kind: InputVideo, Status: InputReady, MediaID: "71", DurationMs: &d2, HasAudio: &yes, SourceNodeID: "a"},
		{No: "2", Kind: InputVideo, Status: InputReady, MediaID: "72", DurationMs: &d0},
		{No: "3", Kind: InputImage, Status: InputReady, MediaID: "73"},
		{No: "4", Kind: InputAudio, Status: InputReady, MediaID: "74", DurationMs: &d2},
		{No: "5", Kind: InputText, Status: InputReady, Text: "解说稿"},
		{No: "6", Kind: InputVideo, Status: InputPending},
		{No: "7", Kind: InputShotGroup, Status: InputReady},
		{No: "8", Kind: InputImage, Status: InputReady},
	}
	cards, skipped := CardsFromInputs(items, map[string]string{"1": " 产品特写 "})
	var nos []string
	for _, c := range cards {
		nos = append(nos, c.No)
	}
	if strings.Join(nos, ",") != "1,3,4,5" {
		t.Fatalf("只收就绪且可用的：%v", nos)
	}
	if cards[0].Description != "产品特写" || !cards[0].HasAudio || cards[0].MediaID != 71 || cards[2].AudioRole != RoleMusic {
		t.Fatalf("%+v", cards)
	}
	if strings.Join(skipped, "|") != "编号 2 的时长未知，没有用上|编号 8 还没有可用的媒体，没有用上" {
		t.Fatalf("说明没收进来的：%v", skipped)
	}
}
