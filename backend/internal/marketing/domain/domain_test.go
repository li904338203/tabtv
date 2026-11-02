package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time { v := t0.Add(d); return &v }

func TestVisible_边界(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		start, end *time.Time
		want       bool
	}{
		{"两端开放", true, nil, nil, true},
		{"start==now 在投", true, at(0), nil, true},
		{"start>now 未开始", true, at(time.Second), nil, false},
		{"end==now 已结束", true, nil, at(0), false},
		{"end>now 在投", true, nil, at(time.Nanosecond), true},
		{"窗口内", true, at(-time.Hour), at(time.Hour), true},
		{"停用恒不可见", false, nil, nil, false},
		{"停用且在窗口内仍不可见", false, at(-time.Hour), at(time.Hour), false},
	}
	for _, c := range cases {
		o := Option{IsEnabled: c.enabled, StartAt: c.start, EndAt: c.end}
		if got := Visible(o, t0); got != c.want {
			t.Errorf("%s: Visible=%v want %v", c.name, got, c.want)
		}
	}
}

func TestLifecycle_四态(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		start, end *time.Time
		want       LifecycleState
	}{
		{"在投", true, at(-time.Hour), at(time.Hour), LifecycleLive},
		{"start==now 在投", true, at(0), at(time.Hour), LifecycleLive},
		{"未开始", true, at(time.Hour), at(2 * time.Hour), LifecycleNotStarted},
		{"end==now 已结束", true, at(-time.Hour), at(0), LifecycleEnded},
		{"已结束", true, nil, at(-time.Hour), LifecycleEnded},
		{"停用恒 disabled（窗口外）", false, nil, at(-time.Hour), LifecycleDisabled},
		{"停用恒 disabled（窗口内）", false, nil, nil, LifecycleDisabled},
	}
	for _, c := range cases {
		o := Option{IsEnabled: c.enabled, StartAt: c.start, EndAt: c.end}
		got := Lifecycle(o, t0)
		if got != c.want {
			t.Errorf("%s: %s want %s", c.name, got, c.want)
		}
		if (got == LifecycleLive) != Visible(o, t0) {
			t.Errorf("%s: Lifecycle 与 Visible 不一致", c.name)
		}
	}
}

func validCreative() Option {
	return Option{
		Kind: KindCreative, Code: "giant_drop", Name: "巨物从天而降", CategoryCode: "big_scene",
		Description: "商品放大到城市尺度后落地展示", CoverMediaIDs: []int64{1, 2, 3},
		Prompt: "商品从天而降", OutputTypes: []string{OutputVideo},
		InputRequirements: map[string]string{SlotProduct: RequirementRequired},
		RecommendedModels: []string{"seedance_2_5"},
		RecommendedParams: json.RawMessage(`{"aspectRatio":"9:16","durationSec":[10,15]}`),
		IsEnabled:         true, Version: 1,
	}
}

func fieldsOf(issues []Issue) map[string]IssueCode {
	m := map[string]IssueCode{}
	for _, i := range issues {
		m[i.Field] = i.Code
	}
	return m
}

func TestValidateOption_一次报完(t *testing.T) {
	if issues := ValidateOption(validCreative()); len(issues) != 0 {
		t.Fatalf("合法选项不应有问题：%+v", issues)
	}
	o := validCreative()
	o.Code = "Bad-Code"
	o.Name = strings.Repeat("名", MaxOptionName+1)
	o.Description = strings.Repeat("说", MaxOptionDescription+1)
	o.Prompt = strings.Repeat("词", MaxOptionPrompt+1)
	o.StartAt, o.EndAt = at(time.Hour), at(0)
	o.OutputTypes = []string{"audio"}
	o.CategoryCode = ""
	issues := ValidateOption(o)
	got := fieldsOf(issues)
	want := map[string]IssueCode{
		"code": IssueBadFormat, "name": IssueTooLong, "description": IssueTooLong, "prompt": IssueTooLong,
		"endAt": IssueBadWindow, "outputTypes[0]": IssueBadValue, "categoryCode": IssueRequired,
	}
	for f, c := range want {
		if got[f] != c {
			t.Errorf("字段 %s：got %q want %q（全部问题：%+v）", f, got[f], c, issues)
		}
	}
	if !HasError(issues) {
		t.Fatal("应有 error")
	}

	// 码点计长：恰好上限的汉字不报。
	ok := validCreative()
	ok.Name = strings.Repeat("名", MaxOptionName)
	ok.Description = strings.Repeat("说", MaxOptionDescription)
	if issues := ValidateOption(ok); len(issues) != 0 {
		t.Fatalf("恰好上限（按码点）不应报：%+v", issues)
	}
}

func TestValidateOption_时段倒置与零长度(t *testing.T) {
	for name, w := range map[string][2]*time.Time{
		"倒置":  {at(time.Hour), at(0)},
		"零长度": {at(0), at(0)},
	} {
		o := validCreative()
		o.StartAt, o.EndAt = w[0], w[1]
		issues := ValidateOption(o)
		if fieldsOf(issues)["endAt"] != IssueBadWindow {
			t.Fatalf("%s 应报 bad_window：%+v", name, issues)
		}
		for _, i := range issues {
			if i.Field == "endAt" && !strings.Contains(i.Message, "结束时间必须晚于开始时间") {
				t.Fatalf("%s 的说明不清楚：%s", name, i.Message)
			}
		}
	}
	o := validCreative()
	o.StartAt, o.EndAt = at(0), at(time.Nanosecond)
	if issues := ValidateOption(o); len(issues) != 0 {
		t.Fatalf("1ns 的窗口是合法的：%+v", issues)
	}
}

func TestValidateOption_kind专属字段(t *testing.T) {
	o := validCreative()
	o.Kind = KindHook
	o.CoverMediaIDs = []int64{1}
	o.DefaultSkillCode = "marketing_video"
	got := fieldsOf(ValidateOption(o))
	for _, f := range []string{"recommendedModels", "recommendedParams", "defaultSkillCode"} {
		if got[f] != IssueKindOnly {
			t.Errorf("hook 上的 %s 应报 kind_only，got %q", f, got[f])
		}
	}

	style := Option{Kind: KindStyle, Code: "warm_light", Name: "暖调", CategoryCode: "light",
		CoverMediaIDs: []int64{1}, Prompt: "暖调", OutputTypes: []string{OutputVideo}, IsAudioRequired: true, IsEnabled: true}
	if fieldsOf(ValidateOption(style))["isAudioRequired"] != IssueKindOnly {
		t.Error("风格不能依赖出声")
	}
	style.IsAudioRequired = false
	style.RecommendedParams = json.RawMessage(`{}`)
	if issues := ValidateOption(style); len(issues) != 0 {
		t.Fatalf("空对象的推荐参数视为未填：%+v", issues)
	}

	// 封面：上限任何时候都拦；下限只在启用时强制。
	c := validCreative()
	c.CoverMediaIDs = []int64{1, 2, 3, 4}
	if fieldsOf(ValidateOption(c))["coverMediaIds"] != IssueCoverCount {
		t.Error("创意 4 张封面应拦")
	}
	style.CoverMediaIDs = []int64{1, 2}
	if fieldsOf(ValidateOption(style))["coverMediaIds"] != IssueCoverCount {
		t.Error("风格 2 张封面应拦")
	}
	draft := validCreative()
	draft.IsEnabled = false
	draft.CoverMediaIDs = nil
	draft.Prompt = ""
	if issues := ValidateOption(draft); len(issues) != 0 {
		t.Fatalf("停用草稿可以没有封面与片段：%+v", issues)
	}
	draft.IsEnabled = true
	got = fieldsOf(ValidateOption(draft))
	if got["coverMediaIds"] != IssueCoverCount || got["prompt"] != IssueRequired {
		t.Fatalf("启用时须有封面与片段：%+v", got)
	}

	// 推荐参数格式与输入要求。
	bad := validCreative()
	bad.RecommendedParams = json.RawMessage(`{"durationSec":[15,10]}`)
	bad.InputRequirements = map[string]string{"avatar": RequirementRequired, SlotCharacter: "must"}
	got = fieldsOf(ValidateOption(bad))
	if got["recommendedParams"] != IssueBadFormat || got["inputRequirements.avatar"] != IssueBadValue || got["inputRequirements.character"] != IssueBadValue {
		t.Fatalf("推荐参数 / 输入要求校验缺失：%+v", got)
	}
}

func TestCompose_空段跳过(t *testing.T) {
	snap := func(k Kind, p string) Snapshot {
		return Snapshot{Pick: Pick{Kind: k, Code: "x_" + string(k), Version: 1}, Prompt: p}
	}
	cases := []struct {
		name       string
		snaps      []Snapshot
		constraint string
		want       string
	}{
		{"全空", nil, "", ""},
		{"只有约束（什么都没选也追加）", nil, "约束", "约束"},
		{"Hook 为空", []Snapshot{snap(KindCreative, "创意"), snap(KindHook, "  "), snap(KindStyle, "风格")}, "约束", "创意\n风格\n约束"},
		{"乱序入参", []Snapshot{snap(KindStyle, "风格"), snap(KindHook, "钩子"), snap(KindCreative, "创意")}, "", "创意\n钩子\n风格"},
		{"两端空白去掉", []Snapshot{snap(KindCreative, "\n创意\n")}, "\n约束 ", "创意\n约束"},
	}
	for _, c := range cases {
		got := Compose(c.snaps, c.constraint)
		if got != c.want {
			t.Errorf("%s: %q want %q", c.name, got, c.want)
		}
		if strings.Contains(got, "\n\n") || strings.HasPrefix(got, "\n") || strings.HasSuffix(got, "\n") {
			t.Errorf("%s: 不得留空行：%q", c.name, got)
		}
	}
}

func TestMergePins_同code不重钉(t *testing.T) {
	existing := []Pick{{KindCreative, "giant_drop", 3}, {KindHook, "ask_open", 1}}
	incoming := []Pick{{KindCreative, "giant_drop", 5}, {KindStyle, "warm_light", 2}, {KindStyle, "warm_light", 9}}
	got := MergePins(existing, incoming)
	want := []Pick{{KindCreative, "giant_drop", 3}, {KindHook, "ask_open", 1}, {KindStyle, "warm_light", 2}}
	if mustJSON(t, got) != mustJSON(t, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if existing[0].Version != 3 || len(existing) != 2 {
		t.Fatal("不得改入参")
	}
	// 同 code 不同 kind 是两个钉（code 全局唯一，正常不会发生；这里只验 PinKey 含 kind）。
	if PinKey(KindHook, "a") == PinKey(KindStyle, "a") {
		t.Fatal("PinKey 必须含 kind")
	}
}

func TestParsePicks(t *testing.T) {
	picks, present, err := ParsePicks(json.RawMessage(`{"prompt":"x"}`))
	if err != nil || present || picks != nil {
		t.Fatalf("没有 marketing 段：present=false，got %v %v %v", picks, present, err)
	}
	picks, present, err = ParsePicks(json.RawMessage(`{"marketing":{"picks":[]}}`))
	if err != nil || !present || len(picks) != 0 {
		t.Fatalf("空 picks 也算存在（要拼约束段）：%v %v %v", picks, present, err)
	}
	picks, present, err = ParsePicks(json.RawMessage(`{"marketing":{"picks":[
		{"kind":"creative","code":"giant_drop","version":3},
		{"kind":"creative","code":"giant_drop","version":4},
		{"kind":"hook","code":"ask_open","version":1}]}}`))
	if err != nil || !present || len(picks) != 2 || picks[0].Version != 3 {
		t.Fatalf("同 kind+code 只留第一次：%v %v %v", picks, present, err)
	}
	for _, bad := range []string{
		`{"marketing":{"picks":[{"kind":"avatar","code":"abc","version":1}]}}`,
		`{"marketing":{"picks":[{"kind":"hook","code":"A","version":1}]}}`,
		`{"marketing":{"picks":[{"kind":"hook","code":"abc","version":0}]}}`,
		`{"marketing":"x"}`,
	} {
		if _, _, err := ParsePicks(json.RawMessage(bad)); err == nil {
			t.Errorf("应报错：%s", bad)
		}
	}
	if _, present, err := ParsePicks(nil); present || err != nil {
		t.Fatal("空 params 不是营销生成")
	}
}

func TestBlockReason_智能匹配不判出声(t *testing.T) {
	yes, no := true, false
	hook := Option{Kind: KindHook, IsAudioRequired: true, OutputTypes: []string{OutputVideo},
		InputRequirements: map[string]string{SlotProduct: RequirementRequired, SlotCharacter: RequirementOptional}}
	cases := []struct {
		name string
		ctx  UsageContext
		want string
	}{
		{"智能匹配（模型未定）不判出声", UsageContext{OutputType: OutputVideo, Slots: map[string]int{SlotProduct: 1}}, ""},
		{"出声模型可用", UsageContext{OutputType: OutputVideo, ModelHasAudio: &yes, Slots: map[string]int{SlotProduct: 1}}, ""},
		{"不出声模型", UsageContext{OutputType: OutputVideo, ModelHasAudio: &no, Slots: map[string]int{SlotProduct: 1}}, BlockNeedsAudio},
		{"缺商品图", UsageContext{OutputType: OutputVideo}, BlockNeedsProduct},
		{"产出不匹配优先于其他", UsageContext{OutputType: OutputImage, ModelHasAudio: &no}, BlockOutputMismatch},
		{"不出声优先于缺图", UsageContext{OutputType: OutputVideo, ModelHasAudio: &no}, BlockNeedsAudio},
		{"产出未知不判产出", UsageContext{Slots: map[string]int{SlotProduct: 2}}, ""},
	}
	for _, c := range cases {
		code, fixable := BlockReason(hook, c.ctx)
		if code != c.want {
			t.Errorf("%s: %q want %q", c.name, code, c.want)
		}
		if (code != "") != fixable {
			t.Errorf("%s: 有原因时必须 userFixable", c.name)
		}
	}
	src := Option{InputRequirements: map[string]string{SlotSourceVideo: RequirementRequired}}
	if code, _ := BlockReason(src, UsageContext{}); code != "needs_source_video" {
		t.Errorf("必填源视频：%q", code)
	}
}

func TestValidateSlotDecl_非法key与accept(t *testing.T) {
	if issues := ValidateSlotDecl(DefaultSlots()); len(issues) != 0 {
		t.Fatalf("默认声明必须合法：%+v", issues)
	}
	decls := []SlotDecl{
		{Key: "avatar", Label: "数字人", Accept: []string{AcceptImage}, MaxCount: 1},
		{Key: SlotProduct, Label: "商品", Accept: []string{AcceptVideo, AcceptImage}, MaxCount: 2,
			Fields: []SlotField{{Key: "price", Label: "价格"}}},
		{Key: SlotProduct, Label: "商品2", Accept: []string{AcceptMarketingProduct}, MaxCount: 1},
		{Key: SlotCharacter, Label: "", Accept: nil, MaxCount: 0},
		{Key: SlotSourceVideo, Label: "源视频", Accept: []string{AcceptVideo}, MaxCount: 2},
	}
	got := fieldsOf(ValidateSlotDecl(decls))
	want := map[string]IssueCode{
		"inputSlots[0].key":           IssueBadValue,
		"inputSlots[1].accept[0]":     IssueBadValue,
		"inputSlots[1].accept[1]":     IssueBadValue, // 未规范化的旧 image：商品槽不收
		"inputSlots[1].maxCount":      IssueBadValue, // 商品槽一次 1 个商品
		"inputSlots[1].fields[0].key": IssueUnknownField,
		"inputSlots[2].key":           IssueDuplicate,
		"inputSlots[3].label":         IssueRequired,
		"inputSlots[3].accept":        IssueRequired,
		"inputSlots[3].maxCount":      IssueBadValue,
		"inputSlots[4].maxCount":      IssueBadValue,
	}
	for f, c := range want {
		if got[f] != c {
			t.Errorf("%s: got %q want %q", f, got[f], c)
		}
	}
	if len(got) != len(want) {
		t.Errorf("多报或少报：%+v", got)
	}
	if SceneSlotMax(SlotProduct) != MaxProductsPerSession || SceneSlotMax(SlotCharacter) != MaxCharacterRefs {
		t.Fatalf("场景上限：商品 %d、角色 %d", SceneSlotMax(SlotProduct), SceneSlotMax(SlotCharacter))
	}
}

// 旧形状（MKT-T-09 录入的技能、seed 数据文件（platform/seed/data/marketing））→ 新形状；新形状原样不动（MKD-26）。
func TestNormalizeSlotDecls_旧形状与幂等(t *testing.T) {
	old := []SlotDecl{
		{Key: SlotSourceVideo, Label: "源视频", Accept: []string{AcceptVideo}, MaxCount: 1},
		{Key: SlotProduct, Label: "商品", Accept: []string{AcceptImage}, Required: true, MaxCount: 20,
			Fields: []SlotField{{Key: "name", Label: "商品名称"}, {Key: "sellingPoints", Label: "卖点"}}},
		{Key: SlotCharacter, Label: "角色", Accept: []string{AcceptImage, AcceptCharacterAsset}, MaxCount: 2},
	}
	got, warns := NormalizeSlotDecls(old)
	want := []SlotDecl{
		{Key: SlotSourceVideo, Label: "源视频", Accept: []string{AcceptVideo}, MaxCount: 1},
		{Key: SlotProduct, Label: "商品", Accept: []string{AcceptMarketingProduct}, Required: true, MaxCount: 1},
		{Key: SlotCharacter, Label: "角色", Accept: []string{AcceptMarketingCharacter, AcceptCharacterAsset}, MaxCount: 2},
	}
	if mustJSON(t, got) != mustJSON(t, want) {
		t.Fatalf("规范化结果\n got %s\nwant %s", mustJSON(t, got), mustJSON(t, want))
	}
	if len(warns) != 1 || warns[0].Field != "inputSlots" || warns[0].Code != IssueNormalized ||
		warns[0].Level != LevelWarn || HasError(warns) {
		t.Fatalf("应恰好一条 inputSlots/normalized 提醒：%+v", warns)
	}
	if issues := ValidateSlotDecl(got); len(issues) != 0 {
		t.Fatalf("规范化后必须合法：%+v", issues)
	}
	if old[1].Accept[0] != AcceptImage || len(old[1].Fields) != 2 {
		t.Fatal("不得改入参")
	}
	// 幂等：新形状再规范化一次原样返回、没有提醒。
	again, w2 := NormalizeSlotDecls(got)
	if mustJSON(t, again) != mustJSON(t, got) || len(w2) != 0 {
		t.Fatalf("新形状应原样、无提醒：%s %+v", mustJSON(t, again), w2)
	}
	if d, w := NormalizeSlotDecls(DefaultSlots()); len(w) != 0 || mustJSON(t, d) != mustJSON(t, DefaultSlots()) {
		t.Fatalf("默认声明本来就是新形状：%+v", w)
	}
	// 重复的 image（脏数据）合并成一个；认不出的槽原样留给 ValidateSlotDecl 报。
	dup, _ := NormalizeSlotDecls([]SlotDecl{{Key: SlotCharacter, Label: "角色",
		Accept: []string{AcceptImage, AcceptMarketingCharacter}, MaxCount: 1}, {Key: "avatar", Accept: []string{AcceptImage}}})
	if mustJSON(t, dup[0].Accept) != `["marketing_character"]` || dup[1].Accept[0] != AcceptImage {
		t.Fatalf("去重 / 未知槽不动：%s", mustJSON(t, dup))
	}
}

// 开发库与数据文件里的 13 个营销技能（platform/seed/data/marketing/options-2026-10-21.json，库里的 input_slots
// 与它逐字一致，2026-10-23 核对）：严格解码 → 规范化 → 校验全部通过。不改库、不改数据文件。
func TestNormalizeSlotDecls_已录入技能可解码(t *testing.T) {
	var data struct {
		Skills []struct {
			Code       string          `json:"code"`
			InputSlots json.RawMessage `json:"inputSlots"`
		} `json:"skills"`
	}
	readJSON(t, "../../platform/seed/data/marketing/options-2026-10-21.json", &data)
	if len(data.Skills) != 13 {
		t.Fatalf("数据文件应有 13 个技能：%d", len(data.Skills))
	}
	products := 0
	for _, sk := range data.Skills {
		var decls []SlotDecl
		dec := json.NewDecoder(bytes.NewReader(sk.InputSlots))
		dec.DisallowUnknownFields() // 与管理端保存同样严格
		if err := dec.Decode(&decls); err != nil {
			t.Fatalf("%s 解码失败：%v", sk.Code, err)
		}
		norm, _ := NormalizeSlotDecls(decls)
		if issues := ValidateSlotDecl(norm); len(issues) != 0 {
			t.Errorf("%s 规范化后不合法：%+v", sk.Code, issues)
		}
		for _, d := range norm {
			if d.Key == SlotProduct {
				products++
				if mustJSON(t, d.Accept) != `["marketing_product"]` || d.MaxCount != 1 || d.Fields != nil {
					t.Errorf("%s 商品槽：%s", sk.Code, mustJSON(t, d))
				}
			}
			if d.Key == SlotCharacter && contains(d.Accept, AcceptImage) {
				t.Errorf("%s 角色槽仍收 image", sk.Code)
			}
		}
	}
	if products != 12 {
		t.Fatalf("12 个技能声明了商品槽：%d", products)
	}
}

func TestValidateSlotFill_点名槽位(t *testing.T) {
	decls := DefaultSlots()
	ok := SlotFill{
		Refs: map[string][]string{SlotProduct: {AcceptMarketingProduct},
			SlotCharacter: {AcceptMarketingCharacter, AcceptCharacterAsset}},
	}
	if issues := ValidateSlotFill(decls, ok); len(issues) != 0 {
		t.Fatalf("合法填充：%+v", issues)
	}
	cases := []struct {
		name  string
		fill  SlotFill
		slot  string
		field string
		code  IssueCode
	}{
		{"必填未填", SlotFill{}, SlotProduct, SlotProduct, IssueRequired},
		{"两个商品", SlotFill{Refs: map[string][]string{SlotProduct: {AcceptMarketingProduct, AcceptMarketingProduct}}}, SlotProduct, SlotProduct, IssueTooMany},
		{"商品槽给了图片", SlotFill{Refs: map[string][]string{SlotProduct: {"image"}}}, SlotProduct, SlotProduct, IssueBadValue},
		{"类型不收", SlotFill{Refs: map[string][]string{SlotProduct: {AcceptMarketingProduct}, SlotCharacter: {"video"}}}, SlotCharacter, SlotCharacter, IssueBadValue},
		{"三个角色", SlotFill{Refs: map[string][]string{SlotProduct: {AcceptMarketingProduct},
			SlotCharacter: {AcceptMarketingCharacter, AcceptMarketingCharacter, AcceptCharacterAsset}}}, SlotCharacter, SlotCharacter, IssueTooMany},
		{"未声明的槽", SlotFill{Refs: map[string][]string{SlotProduct: {AcceptMarketingProduct}, SlotSourceVideo: {"video"}}}, SlotSourceVideo, SlotSourceVideo, IssueBadValue},
		{"字段不再收", SlotFill{Refs: map[string][]string{SlotProduct: {AcceptMarketingProduct}},
			Fields: map[string]map[string]string{SlotProduct: {"name": "杯"}}}, SlotProduct, "product.name", IssueUnknownField},
	}
	for _, c := range cases {
		issues := ValidateSlotFill(decls, c.fill)
		if len(issues) != 1 {
			t.Errorf("%s: 应恰好一条，got %+v", c.name, issues)
			continue
		}
		i := issues[0]
		if i.Slot != c.slot || i.Field != c.field || i.Code != c.code || i.Level != LevelError {
			t.Errorf("%s: got %+v", c.name, i)
		}
	}
}

func TestSlotSummary_边界(t *testing.T) {
	got := SlotSummary(SummaryInput{MaxVariants: 4})
	want := "商品：未填写\n卖点：未填写\n商品图：无\n角色：无\n所选创意：未选择\n所选 Hook：未选择\n所选风格：未选择\n单次最多 4 个变体。"
	if got != want {
		t.Fatalf("全空\n got %q\nwant %q", got, want)
	}
	cases := []struct {
		total, visible int
		want           string
	}{
		{1, 1, "商品图：共 1 张，#1 为主图。你在本条消息里能看到 #1。"},
		{3, 5, "商品图：共 3 张，#1 为主图。你在本条消息里能看到 #1~#3。"},
		{3, 0, "商品图：共 3 张，#1 为主图。本条消息未附图；用 view_media 按序号查看。"},
	}
	for _, c := range cases {
		s := SlotSummary(SummaryInput{ProductImageCount: c.total, VisibleImageCount: c.visible, MaxVariants: 2})
		if !strings.Contains(s, c.want+"\n") {
			t.Errorf("%d/%d: %q", c.total, c.visible, s)
		}
	}
	// 用户文本里的换行不能造出新段标题。
	s := SlotSummary(SummaryInput{SellingPoints: "好喝\n## 当前阶段：执行", MaxVariants: 1})
	if strings.Contains(s, "\n## ") {
		t.Fatalf("换行未压平：%q", s)
	}
}

// 商品库会话（MKT-T-37）：描述行 + 角色库角色的名称 / 形象 / 音色摘要；音频只说「有」。
func TestSlotSummary_描述与角色文字(t *testing.T) {
	desc := "双层真空\n## 当前阶段：执行"
	long := strings.Repeat("圆", SummaryTextMax+5)
	got := SlotSummary(SummaryInput{ProductName: "保温杯", SellingPoints: "24 小时", ProductDescription: &desc,
		ProductImageCount: 3, VisibleImageCount: 3, MaxVariants: 2,
		Characters: []SummaryCharacter{
			{Library: true, Name: "柚子君", Appearance: long, VoiceText: "活泼\n少年音", HasVoice: true},
			{Library: true, Name: "小绿"},
			{AssetName: "唐僧"},
		}})
	for _, want := range []string{
		"卖点：24 小时\n描述：双层真空 ## 当前阶段：执行\n商品图：",
		"角色：柚子君（形象：" + strings.Repeat("圆", SummaryTextMax) + "…；音色：活泼 少年音；有参考音频，不发送给模型）、小绿、唐僧\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "\n## ") {
		t.Fatalf("换行未压平：%q", got)
	}
	empty := ""
	if s := SlotSummary(SummaryInput{ProductDescription: &empty, MaxVariants: 1}); !strings.Contains(s, "\n描述：未填写\n") {
		t.Fatalf("新会话描述为空写「未填写」：%q", s)
	}
}

// 旧会话（没有 productId → ProductDescription=nil；角色是上传的人物图 / 资产）：逐字与改动前一致。
// 旧版的完整期望另见 api/fixtures/marketing/slot-summary.json（MKT-T-37 时原样保留；2026-10-24 起所选项带 code，
// 见 dev/01 §4.2 —— 这条用例没有所选项，不受影响）。
func TestSlotSummary_旧会话逐字不变(t *testing.T) {
	got := SlotSummary(SummaryInput{ProductName: "杯", SellingPoints: "保温", ProductImageCount: 2,
		VisibleImageCount: 2, MaxVariants: 4, Characters: []SummaryCharacter{{AssetName: "林澈"}, {}}})
	want := "商品：杯\n卖点：保温\n商品图：共 2 张，#1 为主图。你在本条消息里能看到 #1~#2。\n" +
		"角色：林澈、上传的人物图 #1\n所选创意：未选择\n所选 Hook：未选择\n所选风格：未选择\n" +
		"单次最多 4 个变体；每个生成项选用的商品图不超过该模型的参考图上限，规划时逐项写明用了哪几张。"
	if got != want {
		t.Fatalf("旧会话摘要变了\n got %q\nwant %q", got, want)
	}
}

func TestSelectImages_越界重复超限(t *testing.T) {
	got, err := SelectImages(20, []int{3, 1}, 4)
	if err != nil || mustJSON(t, got) != "[3,1]" {
		t.Fatalf("合法选择保持顺序：%v %v", got, err)
	}
	cases := []struct {
		name    string
		total   int
		idx     []int
		max     int
		code    string
		mention string
	}{
		{"越界 0", 20, []int{0}, 4, ImageOutOfRange, "#0"},
		{"越界 21", 20, []int{1, 21}, 4, ImageOutOfRange, "#21"},
		{"重复", 20, []int{2, 2}, 4, ImageDuplicate, "#2"},
		{"超上限带 Y", 20, []int{1, 2, 3, 4, 5}, 4, ImageTooMany, "最多 4 张"},
		{"模型不收图", 20, []int{1}, 0, ImageTooMany, "最多 0 张"},
	}
	for _, c := range cases {
		_, err := SelectImages(c.total, c.idx, c.max)
		var se *ImageSelectError
		if !errors.As(err, &se) || se.Code != c.code {
			t.Errorf("%s: got %v", c.name, err)
			continue
		}
		if se.Max != c.max || !strings.Contains(err.Error(), c.mention) {
			t.Errorf("%s: 错误应带 %q 与上限：%v", c.name, c.mention, err)
		}
	}
	got, err = SelectImages(20, nil, 4)
	if err != nil || mustJSON(t, got) != "[1,2,3,4]" {
		t.Fatalf("未指定按默认：%v %v", got, err)
	}
}

func TestDefaultIndexes_取前Y张(t *testing.T) {
	cases := []struct {
		total, max int
		want       string
	}{
		{20, 4, "[1,2,3,4]"},
		{3, 4, "[1,2,3]"},
		{0, 4, "[]"},
		{5, 0, "[]"},
	}
	for _, c := range cases {
		if got := mustJSON(t, DefaultIndexes(c.total, c.max)); got != c.want {
			t.Errorf("%d/%d: %s want %s", c.total, c.max, got, c.want)
		}
	}
}

func TestProductFields_按码点计长度(t *testing.T) {
	name := strings.Repeat("咖", MaxProductName) // 90 字节，30 码点
	sp := strings.Repeat("😀", MaxSellingPoints) // 1200 字节，300 码点
	if len(name) <= MaxProductName || len(sp) <= MaxSellingPoints {
		t.Fatal("用例前提：字节长度超过上限")
	}
	if issues := ValidateProductFields(name, sp); len(issues) != 0 {
		t.Fatalf("按码点恰好上限不应报：%+v", issues)
	}
	issues := ValidateProductFields(name+"啡", sp+"!")
	got := fieldsOf(issues)
	if got["product.name"] != IssueTooLong || got["product.sellingPoints"] != IssueTooLong {
		t.Fatalf("超过一个码点应报：%+v", issues)
	}
}

func TestValidCode(t *testing.T) {
	for code, want := range map[string]bool{
		"abc": true, "giant_drop_2": true, "ab": false, "1abc": false, "Abc": false,
		"a-b-c": false, "a" + strings.Repeat("b", 63): true, "a" + strings.Repeat("b", 64): false,
	} {
		if ValidCode(code) != want {
			t.Errorf("%q: want %v", code, want)
		}
	}
}

// 04 文档 P-48：规划时新钉的选项也要进「## 营销输入」，原先只写建会话时的三项，
// 模型看到「所选 Hook：未选择」而计划里实际用了某个 Hook。
func TestSlotSummary_规划中新钉的选项也列出来(t *testing.T) {
	got := SlotSummary(SummaryInput{ProductName: "保温杯", SellingPoints: "12 小时", ProductImageCount: 1,
		VisibleImageCount: 1, MaxVariants: 4, Options: []SummaryOption{
			{Kind: KindCreative, Name: "痛点立解", Code: "cr_pain_point_fix"},
			{Kind: KindHook, Name: "结果前置", Code: "hk_result_first", FromPlan: true},
			{Kind: KindStyle, Name: "st_gone", Code: "st_gone", FromPlan: true}, // 版本取不到时名称退回 code
		}})
	for _, want := range []string{"所选创意：痛点立解（code cr_pain_point_fix）\n", "所选 Hook：未选择",
		"规划中用到的 Hook：结果前置（code hk_result_first）\n", "规划中用到的风格：st_gone\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("应包含 %q：\n%s", want, got)
		}
	}
}
