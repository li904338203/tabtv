//go:build integration

// 技能场景扩展（MKT-T-05）的真库测试：新列整份写入后逐字不丢、旧表单不带新列不清空、
// 用户端按场景过滤（种子里的 3 条通用技能照旧出现）。槽位与口播校验用真实的 wiring 适配器。
package integration

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// mktSlots 是新形状（MKT-T-37，MKD-26）：往返逐字不变。旧形状保存时会被规范化成新形状，
// 那一条在 TestSkillAdmin_旧形状槽位规范化 里验。
const mktSlots = `[{"key":"product","label":"商品","accept":["marketing_product"],"required":true,"maxCount":1},` +
	`{"key":"character","label":"角色","accept":["marketing_character","character_asset"],"required":false,"maxCount":2}]`

// legacySlots 是 MKT-T-09 录入时的旧形状（库里与 seed 数据文件（platform/seed/data/marketing）里还是它）。
const legacySlots = `[{"key":"product","label":"商品","accept":["image"],"required":true,"maxCount":20,` +
	`"fields":[{"key":"name","label":"商品名称","maxLen":30},{"key":"sellingPoints","label":"卖点","maxLen":300}]},` +
	`{"key":"character","label":"角色","accept":["image","character_asset"],"required":false,"maxCount":2}]`

func sceneSkillAdmin(t *testing.T) *agentapp.SkillAdmin {
	t.Helper()
	a, _ := newSkillAdmin(t)
	a.SetMarketingChecks(wiring.AgentSkillCopy{}, wiring.AgentSkillSlots{})
	return a
}

func jsonEq(t *testing.T, label string, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s 不是合法 JSON：%s", label, got)
	}
	_ = json.Unmarshal([]byte(want), &w)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s 回读不一致：\n得到 %s\n期望 %s", label, got, want)
	}
}

func TestSkillAdmin_新列往返不丢(t *testing.T) {
	a := sceneSkillAdmin(t)
	ctx := context.Background()
	in := skillIn("mkt_roundtrip")
	pre := "上传商品图，说说你想要的风格"
	in.Scenes, in.InputSlots, in.PrefillText = []string{"marketing"}, json.RawMessage(mktSlots), &pre
	batch := true
	in.IsBatch = &batch // 00062（MKT-T-32）
	s, err := a.Create(ctx, 1, in)
	if err != nil {
		t.Fatal(err)
	}
	// 带全字段整份更新后回读，三列逐字不变。
	in.Name = "改过的名字"
	out, err := a.Update(ctx, 1, s.ID, s.Version, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, "scenes", got.Skill.Scenes, `["marketing"]`)
	jsonEq(t, "inputSlots", got.Skill.InputSlots, mktSlots)
	if got.Skill.PrefillText != pre || got.Skill.Name != "改过的名字" || got.Skill.Version != out.Version {
		t.Fatalf("回读不对：%q %q v%d", got.Skill.PrefillText, got.Skill.Name, got.Skill.Version)
	}
	// 不认识新字段的旧表单只改名：三列保留。
	old := skillIn("mkt_roundtrip")
	old.Name = "旧表单改名"
	if _, err := a.Update(ctx, 1, s.ID, got.Skill.Version, old); err != nil {
		t.Fatal(err)
	}
	got, _ = a.Get(ctx, s.ID)
	jsonEq(t, "inputSlots", got.Skill.InputSlots, mktSlots)
	if got.Skill.PrefillText != pre || !got.Skill.IsBatch {
		t.Fatalf("旧表单清掉了预填或批量标识：%q batch=%v", got.Skill.PrefillText, got.Skill.IsBatch)
	}
	// 列表（ListAllSkills）同样读出新列。
	list, _ := a.List(ctx)
	for _, x := range list {
		if x.Code == "mkt_roundtrip" {
			jsonEq(t, "列表 scenes", x.Scenes, `["marketing"]`)
		}
	}
}

// 旧形状经管理端保存成功、回 normalized 提醒、落库与回读都是新形状（MKD-26）；
// 库里直接存着的旧形状（不经保存）在管理端读路径上也按新形状下发。
func TestSkillAdmin_旧形状槽位规范化(t *testing.T) {
	a, r := newSkillAdmin(t)
	a.SetMarketingChecks(wiring.AgentSkillCopy{}, wiring.AgentSkillSlots{})
	ctx := context.Background()
	in := skillIn("mkt_legacy_slots")
	in.Scenes, in.InputSlots = []string{"marketing"}, json.RawMessage(legacySlots)
	s, err := a.Create(ctx, 1, in)
	if err != nil {
		t.Fatalf("旧形状应保存成功：%v", err)
	}
	normalized := 0
	for _, w := range s.Warnings {
		if w.Field == "inputSlots" && w.Code == agentapp.SlotDeclNormalizedCode {
			normalized++
		}
	}
	if normalized != 1 {
		t.Fatalf("应回一条 inputSlots/normalized 提醒：%+v", s.Warnings)
	}
	jsonEq(t, "保存结果", s.Skill.InputSlots, mktSlots)
	// 绕过保存直接写库（模拟 MKT-T-09 录入的历史数据）：管理端详情 / 列表照样给新形状。
	if _, err := r.Pool().Exec(ctx, `update agent_skill set input_slots = $2::jsonb where id = $1`,
		s.Skill.ID, legacySlots); err != nil {
		t.Fatal(err)
	}
	got, err := a.Get(ctx, s.Skill.ID)
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, "详情", got.Skill.InputSlots, mktSlots)
	list, _ := a.List(ctx)
	for _, x := range list {
		if x.Code == "mkt_legacy_slots" {
			jsonEq(t, "列表", x.InputSlots, mktSlots)
		}
	}
}

func TestListEnabledSkills_按场景过滤(t *testing.T) {
	a, r := newSkillAdmin(t)
	a.SetMarketingChecks(wiring.AgentSkillCopy{}, wiring.AgentSkillSlots{})
	ctx := context.Background()
	mk := func(code string, scenes []string) {
		in := skillIn(code)
		in.Scenes = scenes
		in.Bindings.Reasoning = "chat_agent"
		s, err := a.Create(ctx, 1, in)
		if err != nil {
			t.Fatal(err)
		}
		if len(scenes) > 0 && scenes[len(scenes)-1] == "marketing" {
			// 营销技能启用前要有封面（MKT-D-14）：测试里直接写库补一个。
			if _, err := r.Pool().Exec(ctx, `update agent_skill set cover_media_id = 1 where id = $1`, s.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := a.SetEnabled(ctx, 1, s.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	mk("only_general", nil)
	mk("only_marketing", []string{"marketing"})
	mk("both_scenes", []string{"general", "marketing"})

	svc := agentapp.NewService(r, func() int64 { return 1 }, agentapp.ServicePorts{
		SkillModels: itSkillModels{{Code: "chat_agent", Name: "推理模型甲", Domain: "text"}},
	}, nil)
	codes := func(scene string) map[string]agentapp.SkillListItem {
		items, err := svc.SkillsInScene(ctx, scene)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]agentapp.SkillListItem{}
		for _, it := range items {
			m[it.Code] = it
		}
		return m
	}
	all, _ := r.ListEnabledSkills(ctx)
	general := codes("general")
	// 种子里的通用技能照旧出现（创造页回归）：general = 全部启用技能 − 仅属营销的那一条。
	if len(general) != len(all)-1 {
		t.Fatalf("general 应列出 %d 条，得到 %d", len(all)-1, len(general))
	}
	if _, ok := general["only_marketing"]; ok {
		t.Fatal("仅属营销的技能出现在了 general")
	}
	for _, c := range []string{"only_general", "both_scenes"} {
		if _, ok := general[c]; !ok {
			t.Fatalf("general 缺少 %s", c)
		}
	}
	mkt := codes("marketing")
	if len(mkt) != 2 || mkt["only_marketing"].Code == "" || mkt["both_scenes"].Code == "" {
		t.Fatalf("marketing 应只列两条含营销的技能：%v", mkt)
	}
	// 绑定模型带显示名；取不到名退回 code。
	rm := mkt["only_marketing"].Reasoning
	if rm == nil || rm.Code != "chat_agent" || rm.Name != "推理模型甲" {
		t.Fatalf("推理模型显示名不对：%+v", rm)
	}
	bare := agentapp.NewService(r, func() int64 { return 1 }, agentapp.ServicePorts{}, nil)
	items, err := bare.SkillsInScene(ctx, "marketing")
	if err != nil || len(items) != 2 || items[0].Reasoning == nil || items[0].Reasoning.Name != "chat_agent" {
		t.Fatalf("取不到显示名时应退回 code：%v %+v", err, items)
	}
}
