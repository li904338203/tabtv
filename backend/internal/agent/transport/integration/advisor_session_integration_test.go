//go:build integration

// 生意增长顾问的建会话（MKT-T-26，B-16）：纯咨询会不会被场景默认的「商品必填」挡住。
//
// 结论：**会**，如果技能不声明输入槽 —— 未声明（或声明成 `[]`）一律退回场景默认声明，
// 而默认里商品是必填的。解法是**配置不是代码**：顾问技能自己声明 `input_slots`，商品槽 required=false。
// 这里经管理端服务（含营销槽位校验）建技能，再走真实的 POST /agent/sessions 钉住两个方向。
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

type advisorSkillModels struct{}

func (advisorSkillModels) SkillModels(context.Context) ([]agentapp.SkillModelInfo, error) {
	return nil, nil
}

// advisorSessionSlots：商品可选（03 S-13「商品可、文字必」），角色可选。
// ⚠️ 故意保留**旧形状**（与库里 MKT-T-09 录入的那条一致）：经管理端保存时规范化成新形状（MKD-26），
// 新请求的 marketing_product 照常通过。
const advisorSessionSlots = `[{"key":"product","label":"商品","accept":["image"],"required":false,"maxCount":20,` +
	`"fields":[{"key":"name","label":"商品名称"},{"key":"sellingPoints","label":"卖点"}]},` +
	`{"key":"character","label":"角色","accept":["image","character_asset"],"required":false,"maxCount":2}]`

// mkAdvisorSkill 经管理端服务建一条营销技能并启用；slots 为 nil 时不声明输入槽。
func (m *mktEnv) mkAdvisorSkill(code string, slots json.RawMessage) {
	m.t.Helper()
	ctx := context.Background()
	a := agentapp.NewSkillAdmin(m.repo, advisorSkillModels{}, wiring.AgentSkillRenderer{}, m.next)
	a.SetMarketingChecks(wiring.AgentSkillCopy{}, wiring.AgentSkillSlots{})
	saved, err := a.Create(ctx, 1, agentapp.SkillInput{
		Code: code, Name: "生意增长顾问", Category: "营销洞察",
		Body:   "当前阶段 {{state}}。先问清生意情况；不编造行业数据，涉及时写明「需你核实」；不承诺 ROI。",
		Steps:  []domain.SkillStep{{No: 1, Name: "问诊"}, {No: 2, Name: "分析与建议"}},
		Tools:  []string{"read_canvas", "read_doc", "write_doc", "ask_user", "quote", "submit_plan", "generate"},
		Scenes: []string{"marketing"}, InputSlots: slots, CoverMediaID: m.next(),
	})
	if err != nil {
		m.t.Fatalf("经管理端建技能 %s 失败：%v", code, err)
	}
	if _, err := a.SetEnabled(ctx, 1, saved.Skill.ID, true); err != nil {
		m.t.Fatalf("启用技能 %s 失败：%v", code, err)
	}
}

// consultBody 是一份纯咨询的建会话请求：只有文字，没有任何槽位引用。
func consultBody(skill string) map[string]any {
	b := map[string]any{
		"createProject": map[string]any{"name": "增长咨询"},
		"scene":         "marketing",
		"message":       map[string]any{"text": "我卖手工皂，月销 200 单，想提升销量，帮我分析人群和内容方向。"},
	}
	if skill != "" {
		b["skillCode"] = skill
	}
	return b
}

func TestAdvisor_纯咨询无商品图可建会话(t *testing.T) {
	m := newMktEnv(t, true)
	ctx := context.Background()
	m.mkAdvisorSkill("mk_growth_advisor", json.RawMessage(advisorSessionSlots))
	m.mkAdvisorSkill("mk_advisor_noslots", nil)
	m.mkAdvisorSkill("mk_advisor_empty", json.RawMessage(`[]`))

	// ① 反证：不选技能 / 技能没声明槽位 / 声明成 []，都按场景默认声明 —— 商品必填，纯咨询被挡（点名商品槽）。
	for i, skill := range []string{"", "mk_advisor_noslots", "mk_advisor_empty"} {
		r := m.do("POST", "/agent/sessions", m.owner, "k-adv-block-"+idS(int64(i)), consultBody(skill))
		if r.status != http.StatusBadRequest || r.code() != int(errcode.InvalidArgument) || detail(r, "slot") != "product" {
			t.Fatalf("技能 %q 未声明商品可选时应 400 且点名商品槽：%d %v", skill, r.status, r.body)
		}
	}
	if n := m.count(`select count(*) from agent_session`); n != 0 {
		t.Fatalf("被挡住时不应建会话：%d", n)
	}

	// ② 顾问声明商品可选：只有文字也能建会话（不需要任何代码特例）。
	r := m.do("POST", "/agent/sessions", m.owner, "k-adv-ok-0001", consultBody("mk_growth_advisor"))
	if r.status != http.StatusCreated {
		t.Fatalf("顾问纯咨询应 201：%d %v", r.status, r.body)
	}
	sid := r.body["sessionId"].(string)
	var scene string
	var skills []string
	var si agentapp.SceneInput
	if err := m.pool.QueryRow(ctx, `select scene, skill_codes, scene_input from agent_session where id = $1`, sid).
		Scan(&scene, &skills, &si); err != nil {
		t.Fatal(err)
	}
	if scene != "marketing" || len(skills) != 1 || skills[0] != "mk_growth_advisor" || len(si.Products) != 0 ||
		si.Product.Name != "" {
		t.Fatalf("顾问会话应为营销场景、挂着顾问技能、没有商品：%s %v %+v", scene, skills, si)
	}
	// 建会话本身不扣费（第一轮的费用在 Worker 跑轮时按 agent_chat 扣）。
	if n := m.count(`select count(*) from credit_transaction where type = 'consume'`); n != 0 {
		t.Fatalf("建会话不应扣费：%d", n)
	}

	// ③ 商品是「可」不是「不收」：选一个商品库商品照样能建；旧的「只填商品名」形状被拒（MKD-27）。
	b := consultBody("mk_growth_advisor")
	b["message"].(map[string]any)["refs"] = []any{ref("marketing_product", m.product, "product", 0)}
	if r := m.do("POST", "/agent/sessions", m.owner, "k-adv-ok-0003", b); r.status != http.StatusCreated {
		t.Fatalf("顾问选一个商品应 201：%d %v", r.status, r.body)
	}
	b = consultBody("mk_growth_advisor")
	b["marketing"] = map[string]any{"product": map[string]any{"name": "手工皂"}}
	if r := m.do("POST", "/agent/sessions", m.owner, "k-adv-legacy", b); r.status != http.StatusBadRequest ||
		detail(r, "code") != "legacy_shape" {
		t.Fatalf("旧形状 marketing.product 应 400 legacy_shape：%d %v", r.status, r.body)
	}
	// 管理端读到的是新形状。
	var slots string
	_ = m.pool.QueryRow(ctx, `select input_slots::text from agent_skill where code = 'mk_growth_advisor'`).Scan(&slots)
	if !strings.Contains(slots, "marketing_product") || strings.Contains(slots, "sellingPoints") {
		t.Fatalf("经管理端保存的应是新形状：%s", slots)
	}
	// 库里直接存着旧形状（MKT-T-09 录入的历史数据）：`/agent/skills` 下发的仍是新形状，且照样能建会话。
	if _, err := m.pool.Exec(ctx, `update agent_skill set input_slots = $1::jsonb where code = 'mk_growth_advisor'`,
		advisorSessionSlots); err != nil {
		t.Fatal(err)
	}
	ls := m.do("GET", "/agent/skills?scene=marketing", m.owner, "", nil)
	raw, _ := json.Marshal(ls.body["items"])
	if ls.status != http.StatusOK || !strings.Contains(string(raw), `"accept":["marketing_product"]`) ||
		strings.Contains(string(raw), `"accept":["image"]`) || strings.Contains(string(raw), "sellingPoints") {
		t.Fatalf("/agent/skills 应下发新形状：%d %s", ls.status, raw)
	}
	b = consultBody("mk_growth_advisor")
	b["message"].(map[string]any)["refs"] = []any{ref("marketing_product", m.product, "product", 0)}
	if r := m.do("POST", "/agent/sessions", m.owner, "k-adv-ok-0004", b); r.status != http.StatusCreated {
		t.Fatalf("库里是旧形状的技能也应按新形状校验通过：%d %v", r.status, r.body)
	}
	// 「文字必」由通用规则保证：没有文字也没有引用的消息照样被拒。
	b = consultBody("mk_growth_advisor")
	b["message"] = map[string]any{"text": ""}
	if r := m.do("POST", "/agent/sessions", m.owner, "k-adv-empty", b); r.status != http.StatusBadRequest {
		t.Fatalf("空消息应 400：%d %v", r.status, r.body)
	}
}
