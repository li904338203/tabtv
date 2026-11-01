//go:build integration

package integration

import (
	"context"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// MCPT-04 models（MCP-F-27）：**不可用的模型照样列出并带原因码**。
// 只列可用的（UsableModels 的口径）的话，外部模型会以为平台没有这个模型 ——
// 而其实只差会员等级或运营配一个价。可用性走 QuoteWithFacts，与报价/受理同一个答案。
func TestModelsWithAvailability_不可用照样列出并带原因(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()
	for _, code := range []string{"ok-img", "off-img", "noprice-img", "vip-img", "i2v"} {
		status := "enabled"
		if code == "off-img" {
			status = "disabled"
		}
		op := "text2img"
		if code == "i2v" {
			op = "img2video"
		}
		seedModel(t, pool, next, modelSeed{code: code, providerStatus: "enabled", modelStatus: status,
			domain: "image", op: op, opEnabled: true})
		mustExec(t, pool, `update provider_config set auth_type = 'none' where code = $1`, "p-"+code)
		role, min := "text", 0
		if op == "img2video" {
			role, min = "image", 1 // 至少一张图：探测输入必须按能力下限给，否则永远判成不可用
		}
		mustExec(t, pool, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
			select $1, m.id, o.id, $2, $3, 4 from model m, operation o where m.code = $4 and o.code = $5`,
			next(), role, min, code, op)
		if code != "noprice-img" {
			mustExec(t, pool, `insert into pricing (id, operation_id, model_id, credits, unit, status)
				select $1, o.id, m.id, 7, 'per_image', 'enabled' from operation o, model m
				where o.code = $2 and m.code = $3`, next(), op, code)
		}
	}
	levelID := next()
	mustExec(t, pool, `insert into member_level (id, code, name, sort, is_enabled) values ($1,'pro','专业版',20,true)`, levelID)
	mustExec(t, pool, `update model set min_member_level_id = $1 where code = 'vip-img'`, levelID)
	// 一个操作都没绑的模型：照样列出，原因写在模型上。
	mustExec(t, pool, `insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol,
		submit_path, est_seconds, status, sort)
		select $1, 'bare', 'bare', provider_id, 'image', 'up', 'openai_images', '/x', 10, 'enabled', 0
		from model where code = 'ok-img'`, next())

	svc := catalogapp.New(r, next)
	list, err := svc.ModelsWithAvailability(ctx, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]catalogapp.ModelAvailability{}
	for _, m := range list {
		byCode[m.Code] = m
	}
	want := map[string]string{
		"ok-img": "", "off-img": errcode.ModelUnavailable.Name(), "noprice-img": errcode.PricingMissing.Name(),
		"vip-img": errcode.MemberLevelRequired.Name(), "i2v": "",
	}
	for code, blocked := range want {
		m, ok := byCode[code]
		if !ok {
			t.Fatalf("%s 没被列出（不可用的也必须列出）", code)
		}
		if len(m.Operations) != 1 {
			t.Fatalf("%s 操作数 %d", code, len(m.Operations))
		}
		o := m.Operations[0]
		if o.BlockedCode != blocked || o.Available != (blocked == "") || m.Available != (blocked == "") {
			t.Errorf("%s：available=%v blocked=%q（%s），期望 %q", code, o.Available, o.BlockedCode, o.BlockedReason, blocked)
		}
		if blocked == "" && (o.Credits != 7 || o.Unit != "per_image") {
			t.Errorf("%s 单价应来自报价：%+v", code, o)
		}
		if blocked != "" && o.BlockedReason == "" {
			t.Errorf("%s 不可用却没给原因", code)
		}
	}
	if !byCode["vip-img"].Operations[0].UserFixable || byCode["noprice-img"].Operations[0].UserFixable {
		t.Error("会员等级不足用户能解决（升级），缺价要运营处理")
	}
	if b := byCode["bare"]; b.Available || b.BlockedCode == "" || len(b.Operations) != 0 {
		t.Errorf("未绑操作的模型应列出并带原因：%+v", b)
	}

	// 会员够了就能用；筛选按操作码。
	list, err = svc.ModelsWithAvailability(ctx, "", "text2img", levelID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if m.Code == "i2v" || m.Code == "bare" {
			t.Errorf("按 text2img 筛选不该出现 %s", m.Code)
		}
		if m.Code == "vip-img" && !m.Available {
			t.Errorf("会员达标后 vip-img 应可用：%+v", m.Operations)
		}
	}
}
