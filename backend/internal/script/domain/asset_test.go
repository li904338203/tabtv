package domain

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 版式规范

func TestSheetSpec_三类资产各有固定版式(t *testing.T) {
	// ⚠️ 这段由代码固定、**不让模型生成**：让模型写的话它会自作主张
	// 给角色塞武器、给空场景加人 —— 而资产图的全部价值就是下游每一镜
	// 拿它当参考，多出来的东西会被每一镜继承。文字取自老系统（CD-79）。
	character := SheetSpec(AssetCharacter)
	for _, want := range []string{"角色四视图", "面部头部特写", "角色必须空手", "不得出现其他角色"} {
		if !strings.Contains(character, want) {
			t.Fatalf("角色版式缺「%s」: %s", want, character)
		}
	}
	scene := SheetSpec(AssetScene)
	for _, want := range []string{"2 行 2 列", "不得出现任何人物"} {
		if !strings.Contains(scene, want) {
			t.Fatalf("场景版式缺「%s」（不排除人物的话参考图会把人画进去）: %s", want, scene)
		}
	}
	prop := SheetSpec(AssetProp)
	for _, want := range []string{"2 行 3 列", "六视图", "不得出现手、脚"} {
		if !strings.Contains(prop, want) {
			t.Fatalf("道具版式缺「%s」: %s", want, prop)
		}
	}
	for _, spec := range []string{character, scene, prop} {
		if !strings.HasPrefix(spec, sheetMarker) || strings.Contains(spec, "\n") {
			t.Fatalf("版式要以「构图规范：」开头且是一段（摘要按它切）: %q", spec)
		}
	}
}

func TestBuildDescription_照老系统分段且带标签(t *testing.T) {
	// CD-79：此前是「名称 / 外形 / 服装 / 特征」挤在一行，用户分不清哪段是什么、改的时候不知道往哪写。
	got := BuildDescription("唐僧", AssetCharacter, AssetProfile{
		Alias: "金蝉子", Appearance: "二十五岁男性，身高一米七八，面容清俊", Costume: "金色袈裟，灰白僧衣，布鞋",
		Features: "眉心一点红", Layers: "不该出现", Mood: "也不该出现",
	}, "中国传统水墨风格")
	want := "名称：唐僧 金蝉子\n\n描述：二十五岁男性，身高一米七八，面容清俊\n\n服装：金色袈裟，灰白僧衣，布鞋\n\n特征：眉心一点红\n\n" +
		SheetSpec(AssetCharacter) + "\n\n视觉风格：中国传统水墨风格"
	if got != want {
		t.Fatalf("角色描述的结构不对：\n得到 %q\n应为 %q", got, want)
	}
}

func TestBuildDescription_场景写前背景与氛围_道具只写特征(t *testing.T) {
	scene := BuildDescription("白骨洞", AssetScene, AssetProfile{
		Appearance: "山腹中的天然溶洞", Costume: "不该出现", Layers: "背景是骨堆，前景是石阶", Mood: "阴冷",
	}, "")
	for _, want := range []string{"描述：山腹中的天然溶洞", "前背景：背景是骨堆，前景是石阶", "氛围：阴冷"} {
		if !strings.Contains(scene, want) {
			t.Fatalf("场景缺「%s」: %s", want, scene)
		}
	}
	if strings.Contains(scene, "不该出现") || strings.Contains(scene, "视觉风格") {
		t.Fatalf("场景不该带服装；没有全片风格时不写那一段: %s", scene)
	}
	// CD-76 时期的档案把氛围写在 ambience：照样读出来，不凭空丢掉
	legacy := BuildDescription("天台", AssetScene, AssetProfile{Appearance: "老旧楼顶", Ambience: "黄昏，风大"}, "")
	if !strings.Contains(legacy, "氛围：黄昏，风大") {
		t.Fatalf("旧档案的 ambience 要读成氛围: %s", legacy)
	}
	prop := BuildDescription("金箍棒", AssetProp, AssetProfile{Appearance: "两头金箍的铁棒", Costume: "不该出现", Features: "棒身刻字"}, "")
	if strings.Contains(prop, "服装") || !strings.Contains(prop, "特征：棒身刻字") {
		t.Fatalf("道具只写描述与特征: %s", prop)
	}
}

func TestBuildDescription_没有档案时退回模板而不是编造(t *testing.T) {
	// ⚠️ 编造出来的外观会被下游每一镜继承，而它跟剧情毫无关系。
	got := BuildDescription("路人甲", AssetCharacter, AssetProfile{Alias: "只有别称不算档案"}, "")
	if !strings.Contains(got, "描述：剧情中出现的角色，外观未在剧本中明确描述") || !strings.Contains(got, sheetMarker) {
		t.Fatalf("应退回模板描述并照样带版式: %s", got)
	}
}

func TestProfileSummary_只取长什么样(t *testing.T) {
	// ⚠️ 构图规范与排在它后面的视觉风格属于设定图：喂给分镜图提示词的话，
	// 实拍镜头会收到「纯白背景、四视图、角色必须空手」，与镜头本身的场景冲突。
	desc := BuildDescription("唐僧", AssetCharacter, AssetProfile{Appearance: "面容清俊", Costume: "袈裟"}, "水墨")
	if got := ProfileSummary(desc); got != "描述：面容清俊；服装：袈裟" {
		t.Fatalf("摘要应只剩档案部分且压成一行，得到 %q", got)
	}
	// CD-79 之前的单行旧格式也能取
	if got := ProfileSummary("唐僧 / 面容慈悲\n构图规范：单人四视图"); got != "唐僧 / 面容慈悲" {
		t.Fatalf("旧格式描述的摘要不对: %q", got)
	}
}

func TestAssetPrompt_用途在前_描述原样发出(t *testing.T) {
	// 所见即所得：描述里已有版式与视觉风格就原样发，版式只出现一次。
	desc := BuildDescription("林澈", AssetCharacter, AssetProfile{Appearance: "短发"}, "冷色调")
	got := AssetPrompt(Asset{Name: "林澈", Type: AssetCharacter, Description: desc})
	if !strings.HasPrefix(got, "用途：保持该角色跨镜头的身份识别与当前造型。\n") {
		t.Fatalf("出图提示词要先写用途: %s", got)
	}
	if n := strings.Count(got, sheetMarker); n != 1 || !strings.HasSuffix(got, desc) {
		t.Fatalf("描述应原样发出、版式只出现一次（实际 %d 次）: %s", n, got)
	}
}

func TestAssetPrompt_用户手写的描述会补上版式(t *testing.T) {
	got := AssetPrompt(Asset{Name: "林澈", Type: AssetCharacter, Description: "一个中年男人"})
	if !strings.Contains(got, sheetMarker) {
		t.Fatalf("手写描述也要带上版式约束: %s", got)
	}
	if empty := AssetPrompt(Asset{Name: "白骨洞", Type: AssetScene}); !strings.Contains(empty, "主体：白骨洞") || !strings.Contains(empty, "2 行 2 列") {
		t.Fatalf("空描述回落成「主体 + 类型版式」: %s", empty)
	}
}

// ---------------------------------------------------------------- 质量检查

func TestLintAsset_只警告不拦截的三类问题(t *testing.T) {
	// ⚠️ 这几条都是质量问题不是错误 —— 拦下来用户就走不下去了，
	// 而他可能只是还没来得及改。
	cases := []struct {
		name   string
		asset  Asset
		others []string
		want   string
	}{
		{"占位名", Asset{Name: "角色1", Description: "一个人"}, nil, "name_not_semantic"},
		{"表单字段直贴", Asset{Name: "林澈", Description: "身份：女配\n短发"}, nil, "form_field_pasted"},
		{"描述里写了别人", Asset{Name: "林澈", Description: "和苏敏一起出现"}, []string{"苏敏"},
			"other_name_in_description"},
	}
	for _, c := range cases {
		lints := LintAsset(c.asset, c.others)
		var found bool
		for _, l := range lints {
			if l.Code == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: 应报 %s，实际 %+v", c.name, c.want, lints)
		}
	}
}

func TestLintAsset_正常资产没有提示(t *testing.T) {
	// 描述里的段落标签（名称 / 描述 / 构图规范 / 视觉风格…）是代码写的，不算「表单字段直贴」
	a := Asset{Name: "林澈", Description: BuildDescription("林澈", AssetCharacter,
		AssetProfile{Appearance: "三十岁男性，短发", Costume: "风衣", Features: "左眉有疤"}, "冷色调")}
	if l := LintAsset(a, []string{"林澈", "苏敏"}); len(l) != 0 {
		t.Fatalf("正常资产不该有提示: %+v", l)
	}
}

// ---------------------------------------------------------------- reconcile

func TestReconcile_按名字建资产并带上档案(t *testing.T) {
	plan := Reconcile(ReconcileInput{
		Entities: []Entity{{Name: "林澈", Type: AssetCharacter}, {Name: "天台", Type: AssetScene}},
		Profiles: []AssetProfile{{Name: "林澈", Type: AssetCharacter, Appearance: "短发"}},
	})
	if len(plan.Create) != 2 {
		t.Fatalf("应建 2 个资产: %+v", plan.Create)
	}
	// 模型没给档案的那个要进 Unmatched，并退回模板描述。
	if len(plan.Unmatched) != 1 || plan.Unmatched[0] != "天台" {
		t.Fatalf("缺档案的名单不对: %v", plan.Unmatched)
	}
	for _, a := range plan.Create {
		if a.Name == "天台" && !strings.Contains(a.Description, "未在剧本中明确描述") {
			t.Fatalf("缺档案应退回模板: %s", a.Description)
		}
	}
}

func TestReconcile_已出图的资产不被覆盖(t *testing.T) {
	// ⚠️ 重新生成分镜会重跑 reconcile，而用户可能已经把描述调好、
	// 甚至已经花钱出了图。覆盖等于把他的工作删掉。
	plan := Reconcile(ReconcileInput{
		Entities: []Entity{{Name: "林澈", Type: AssetCharacter}},
		Profiles: []AssetProfile{{Name: "林澈", Type: AssetCharacter, Appearance: "全新的外形"}},
		Existing: []Asset{{
			ID: 1, Name: "林澈", Type: AssetCharacter, Description: "用户精心改过的描述",
			Status: AssetReady, MediaObjectID: 99,
		}},
	})
	if len(plan.Update) != 0 {
		t.Fatalf("已 ready 的资产不该被更新: %+v", plan.Update)
	}
	if len(plan.Create) != 0 {
		t.Fatalf("同名资产不该重复创建: %+v", plan.Create)
	}
}

func TestReconcile_上传的资产也不被覆盖(t *testing.T) {
	plan := Reconcile(ReconcileInput{
		Entities: []Entity{{Name: "林澈", Type: AssetCharacter}},
		Profiles: []AssetProfile{{Name: "林澈", Appearance: "新的"}},
		Existing: []Asset{{ID: 1, Name: "林澈", Type: AssetCharacter,
			Source: SourceUpload, Status: AssetPending, Description: "自己写的"}},
	})
	if len(plan.Update) != 0 {
		t.Fatalf("上传来源的资产不该被覆盖: %+v", plan.Update)
	}
}

func TestReconcile_pending的资产会被更新(t *testing.T) {
	plan := Reconcile(ReconcileInput{
		Entities: []Entity{{Name: "林澈", Type: AssetCharacter}},
		Profiles: []AssetProfile{{Name: "林澈", Type: AssetCharacter, Appearance: "新外形"}},
		Existing: []Asset{{ID: 1, Name: "林澈", Type: AssetCharacter,
			Status: AssetPending, Source: SourceAI, Description: "旧的"}},
	})
	if len(plan.Update) != 1 || !strings.Contains(plan.Update[0].Description, "新外形") {
		t.Fatalf("还没出图的资产应当跟着档案更新: %+v", plan.Update)
	}
}

func TestReconcile_不再出现的资产只报不删(t *testing.T) {
	// ⚠️ 用户可能刚删掉某一镜、正准备加回来，而资产图是花钱生成的。
	// 删掉的代价不可逆，留着的代价只是列表长一点。
	plan := Reconcile(ReconcileInput{
		Entities: []Entity{{Name: "林澈", Type: AssetCharacter}},
		Existing: []Asset{
			{ID: 1, Name: "林澈", Type: AssetCharacter, Status: AssetPending},
			{ID: 2, Name: "苏敏", Type: AssetCharacter, Status: AssetReady, MediaObjectID: 9},
		},
	})
	if len(plan.Orphan) != 1 || plan.Orphan[0].Name != "苏敏" {
		t.Fatalf("不再出现的资产应报为 orphan: %+v", plan.Orphan)
	}
}

func TestReconcile_档案里的类型优先于镜头实体(t *testing.T) {
	// 实体的类型可能来自正则兜底（只覆盖得了现代题材），档案里是模型自己标的。
	plan := Reconcile(ReconcileInput{
		Entities: []Entity{{Name: "金箍棒", Type: AssetCharacter}},
		Profiles: []AssetProfile{{Name: "金箍棒", Type: AssetProp}},
	})
	if plan.Create[0].Type != AssetProp {
		t.Fatalf("应按档案里的类型: %s", plan.Create[0].Type)
	}
}

// ---------------------------------------------------------------- 引用排序

func TestOrderRefs_按镜头实体顺序锚定(t *testing.T) {
	// ⚠️ 顺序即锚定：参考图的编号与提示词里提到它们的顺序一一对应。
	// 顺序错了，模型会把 A 的特征画到 B 身上，而画面看上去"挺正常"——
	// 这种错最难被发现。
	byName := map[string]int64{"林澈": 10, "苏敏": 20, "天台": 30}
	refs := OrderRefs([]Entity{{Name: "苏敏"}, {Name: "天台"}, {Name: "林澈"}}, byName)
	if len(refs) != 3 {
		t.Fatalf("应有 3 条引用: %+v", refs)
	}
	if refs[0].AssetID != 20 || refs[1].AssetID != 30 || refs[2].AssetID != 10 {
		t.Fatalf("顺序应跟随实体出现顺序: %+v", refs)
	}
	for i, r := range refs {
		if r.Sort != i+1 {
			t.Fatalf("sort 应从 1 起连续: %+v", refs)
		}
	}
}

func TestOrderRefs_缺资产时跳过而不是留空位(t *testing.T) {
	// 留空位会让后面所有编号错位。
	refs := OrderRefs([]Entity{{Name: "林澈"}, {Name: "还没建的"}, {Name: "苏敏"}},
		map[string]int64{"林澈": 10, "苏敏": 20})
	if len(refs) != 2 || refs[1].Sort != 2 {
		t.Fatalf("应跳过并保持编号连续: %+v", refs)
	}
}

func TestOrderRefs_同名只算一次(t *testing.T) {
	refs := OrderRefs([]Entity{{Name: "林澈"}, {Name: "林澈"}}, map[string]int64{"林澈": 10})
	if len(refs) != 1 {
		t.Fatalf("同名实体只该有一条引用: %+v", refs)
	}
}

// ---------------------------------------------------------------- 批量生成

func TestPlanBatch_余额不足要停掉后续项(t *testing.T) {
	// ⚠️ 逐项失败的话，用户会收到十条一模一样的"积分不足"，
	// 每条还占一次任务记录。停下来并说清"还差多少"才是有用的。
	assets := []Asset{
		{ID: 1, Name: "甲"}, {ID: 2, Name: "乙"}, {ID: 3, Name: "丙"}, {ID: 4, Name: "丁"},
	}
	got := PlanBatch(assets, 10, 25) // 只够两个
	submitted := 0
	for _, d := range got {
		if !d.Skip {
			submitted++
		}
	}
	if submitted != 2 {
		t.Fatalf("25 分按 10 分一个只够 2 个，实际提交 %d", submitted)
	}
	if !got[2].Stopped || !strings.Contains(got[2].Reason, "还需") {
		t.Fatalf("第三个应说清还差多少: %+v", got[2])
	}
	// ⚠️ 后面的项也标 Stopped：它们不是"各自失败"，是被同一个原因挡住的。
	if !got[3].Stopped {
		t.Fatalf("后续项应标为已停止: %+v", got[3])
	}
}

func TestPlanBatch_已就绪的跳过而不是重做(t *testing.T) {
	// ⚠️ 重做要再花一次钱，而用户点"批量生成"的意思是"把没有的补上"。
	got := PlanBatch([]Asset{
		{ID: 1, Name: "甲", Status: AssetReady, MediaObjectID: 9},
		{ID: 2, Name: "乙", Status: AssetPending},
	}, 10, 100)
	if !got[0].Skip || !strings.Contains(got[0].Reason, "已有资产图") {
		t.Fatalf("已就绪应跳过: %+v", got[0])
	}
	if got[1].Skip {
		t.Fatalf("未就绪的应当提交: %+v", got[1])
	}
	// 跳过的不该占用额度：100 分能覆盖剩下那个。
	if got[1].Stopped {
		t.Fatal("跳过的项不该消耗额度")
	}
}

func TestPlanBatch_余额刚好够(t *testing.T) {
	got := PlanBatch([]Asset{{ID: 1}, {ID: 2}}, 10, 20)
	for i, d := range got {
		if d.Skip {
			t.Fatalf("第 %d 项刚好够，不该跳过: %+v", i, d)
		}
	}
}

// ---------------------------------------------------------------- 上传约束

func TestValidateAssetMedia(t *testing.T) {
	if err := ValidateAssetMedia("image/png", 1<<20); err != nil {
		t.Fatalf("PNG 应被接受: %v", err)
	}
	if err := ValidateAssetMedia("image/gif", 1<<20); err == nil {
		t.Fatal("GIF 不在允许列表里")
	}
	err := ValidateAssetMedia("image/png", MaxAssetUploadBytes+1)
	if err == nil {
		t.Fatal("超过 20 MiB 应被拒")
	}
	if !strings.Contains(err.Error(), "20") {
		t.Fatalf("要说清上限是多少: %v", err)
	}
}

func TestValidateAssetName(t *testing.T) {
	if err := ValidateAssetName("林澈"); err != nil {
		t.Fatalf("正常名字应通过: %v", err)
	}
	if err := ValidateAssetName("  "); err == nil {
		t.Fatal("空名应被拒")
	}
	// 与实体名同一个上限：名字要能在镜头描述里 @ 出来。
	if err := ValidateAssetName(strings.Repeat("长", MaxEntityNameRunes+1)); err == nil {
		t.Fatal("超长名应被拒")
	}
}

func TestFingerprintOf_改了描述就是新指纹(t *testing.T) {
	// ⚠️ 用户改描述的意思就是"上次那张不对，重来一张" —— 幂等键必须跟着变。
	a := FingerprintOf("一个短发男人")
	b := FingerprintOf("一个长发男人")
	if a == b {
		t.Fatal("不同描述应产出不同指纹")
	}
	if a != FingerprintOf(" 一个短发男人 ") {
		t.Fatal("首尾空白不该影响指纹")
	}
	if len(a) != 8 {
		t.Fatalf("指纹要短（列宽有限）: %s", a)
	}
}
