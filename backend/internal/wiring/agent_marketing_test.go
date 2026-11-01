package wiring

import (
	"encoding/json"
	"strings"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
)

// 槽位声明经端口调到 marketing/domain.ValidateSlotDecl：key / accept / maxCount 一次报完（MKT-T-05）。
func TestAgentSkillSlots_一次报完(t *testing.T) {
	raw := json.RawMessage(`[
		{"key":"logo","label":"标志","accept":["image"],"maxCount":1},
		{"key":"product","label":"商品","accept":["audio"],"maxCount":1},
		{"key":"character","label":"角色","accept":["marketing_character","character_asset"],"maxCount":3}
	]`)
	_, got := AgentSkillSlots{}.ValidateSlotDecl(raw)
	fields := map[string]bool{}
	for _, i := range got {
		fields[i.Field] = true
	}
	for _, f := range []string{"inputSlots[0].key", "inputSlots[1].accept[0]", "inputSlots[2].maxCount"} {
		if !fields[f] {
			t.Errorf("缺少 %s 的问题：%+v", f, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("应恰好 3 处问题：%+v", got)
	}
	// 拼错的键不能按零值静默存下去。
	if _, got := (AgentSkillSlots{}).ValidateSlotDecl(json.RawMessage(
		`[{"key":"product","label":"商品","accept":["marketing_product"],"max_count":1}]`)); len(got) != 1 || got[0].Field != "inputSlots" {
		t.Fatalf("未知字段应被拒：%+v", got)
	}
	// 落库的是规范化结果：大小写写错的键（Go 解码不区分大小写）被改写成契约里的 maxCount。
	norm, got := (AgentSkillSlots{}).ValidateSlotDecl(json.RawMessage(
		`[{"key":"character","label":"角色","accept":["marketing_character"],"maxcount":2}]`))
	if len(got) != 0 || !strings.Contains(string(norm), `"maxCount":2`) || strings.Contains(string(norm), "maxcount") {
		t.Fatalf("应规范化成 maxCount：%s %+v", norm, got)
	}
}

// 旧形状技能（MKT-T-09 录入：商品槽 accept:["image"]、maxCount:20、fields）保存成功，
// 回一条 code=normalized 的提醒，落库的是新形状（MKD-26）；读路径同一份规范化。
func TestAgentSkillSlots_旧形状规范化(t *testing.T) {
	old := json.RawMessage(`[{"key":"product","label":"商品","accept":["image"],"required":true,"maxCount":20,
		"fields":[{"key":"name","label":"商品名称"},{"key":"sellingPoints","label":"卖点"}]},
		{"key":"character","label":"角色","accept":["image","character_asset"],"maxCount":2}]`)
	norm, got := AgentSkillSlots{}.ValidateSlotDecl(old)
	if len(got) != 1 || got[0].Code != agentapp.SlotDeclNormalizedCode || got[0].Field != "inputSlots" {
		t.Fatalf("旧形状应只回一条 normalized 提醒：%+v", got)
	}
	want := `[{"key":"product","label":"商品","accept":["marketing_product"],"required":true,"maxCount":1},` +
		`{"key":"character","label":"角色","accept":["marketing_character","character_asset"],"required":false,"maxCount":2}]`
	if string(norm) != want {
		t.Fatalf("落库应是新形状\n got %s\nwant %s", norm, want)
	}
	if read := (AgentSkillSlots{}).NormalizeSlotDecl(old); string(read) != want {
		t.Fatalf("读路径与写路径同一份规范化：%s", read)
	}
	if n2, got2 := (AgentSkillSlots{}).ValidateSlotDecl(norm); len(got2) != 0 || string(n2) != want {
		t.Fatalf("新形状再保存无提醒：%s %+v", n2, got2)
	}
	if bad := (AgentSkillSlots{}).NormalizeSlotDecl(json.RawMessage(`{"x":1}`)); string(bad) != `{"x":1}` {
		t.Fatalf("解不开的原样返回：%s", bad)
	}
}

func TestAgentSkillCopy_禁用词拦截且点名字段(t *testing.T) {
	errs, warns := AgentSkillCopy{}.CheckCopy(map[string]string{
		"name": "口播带货", "prefillText": "用数字人对口型讲解",
	}, []string{"口播"})
	if len(errs) != 2 || errs[0].Field != "prefillText" || !strings.Contains(errs[0].Message, "禁用词") {
		t.Fatalf("预填里的两个禁用词应各报一条并点名字段：%+v", errs)
	}
	if len(warns) != 1 || warns[0].Code != "missing_disclaimer" {
		t.Fatalf("缺提示句应告警：%+v", warns)
	}
}

// 建营销会话的槽位校验经端口调到 marketing/domain.ValidateSlotFill（MKT-T-17）：
// 未选技能用场景默认声明；技能声明了就按技能的；每条问题都点名槽位。
func TestAgentScene_槽位按真实规则点名(t *testing.T) {
	sc := AgentScene{}
	has := func(got []agentapp.SlotIssue, slot, code string) bool {
		for _, i := range got {
			if i.Slot == slot && i.Code == code {
				return true
			}
		}
		return false
	}
	// 默认声明：缺商品必填。
	got := sc.ValidateSlotFill(nil, agentapp.SceneSlotFill{Refs: map[string][]string{"character": {"character_asset"}}})
	if !has(got, "product", "required") {
		t.Fatalf("缺商品应点名 product/required：%+v", got)
	}
	// 两个商品、角色给了视频、商品槽带字段：一次报完。
	got = sc.ValidateSlotFill(json.RawMessage(`[]`), agentapp.SceneSlotFill{
		Refs:   map[string][]string{"product": {"marketing_product", "marketing_product"}, "character": {"video"}},
		Fields: map[string]map[string]string{"product": {"sellingPoints": "好"}},
	})
	if !has(got, "product", "too_many") || !has(got, "character", "bad_value") || !has(got, "product", "unknown_field") {
		t.Fatalf("应一次报完三条：%+v", got)
	}
	// 库里的旧形状技能（商品槽 accept:["image"]）：解码处规范化，新请求的 marketing_product 照常通过。
	legacy := json.RawMessage(`[{"key":"product","label":"商品","accept":["image"],"required":true,"maxCount":20,
		"fields":[{"key":"name","label":"商品名称"}]},{"key":"character","label":"角色","accept":["image","character_asset"],"maxCount":2}]`)
	if got = sc.ValidateSlotFill(legacy, agentapp.SceneSlotFill{Refs: map[string][]string{
		"product": {"marketing_product"}, "character": {"marketing_character"}}}); len(got) != 0 {
		t.Fatalf("旧形状技能应按新形状校验：%+v", got)
	}
	if got = sc.ValidateSlotFill(legacy, agentapp.SceneSlotFill{Refs: map[string][]string{
		"product": {"image"}}}); !has(got, "product", "bad_value") {
		t.Fatalf("旧形状技能也不再收商品图：%+v", got)
	}
	// 技能声明了源视频必填：缺了点名 source_video；默认声明里没有的槽不会凭空出现。
	decl := json.RawMessage(`[{"key":"source_video","label":"源视频","accept":["video"],"required":true,"maxCount":1}]`)
	got = sc.ValidateSlotFill(decl, agentapp.SceneSlotFill{})
	if !has(got, "source_video", "required") || has(got, "product", "required") {
		t.Fatalf("应按技能声明校验：%+v", got)
	}
	// 声明损坏：拒绝而不是退回默认声明。
	if got = sc.ValidateSlotFill(json.RawMessage(`{"x":1}`), agentapp.SceneSlotFill{}); len(got) != 1 {
		t.Fatalf("损坏的声明应拒绝：%+v", got)
	}
}
