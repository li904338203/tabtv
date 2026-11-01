package transport

import (
	"encoding/json"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 会话详情的 planProgress（MKT-T-27）：没有时是 null（不是空对象），ID 对外是字符串，字段名与契约一致。
func TestPlanProgressOf_契约形状(t *testing.T) {
	b, _ := json.Marshal(map[string]any{"planProgress": planProgressOf(nil)})
	if string(b) != `{"planProgress":null}` {
		t.Fatalf("没有已批准计划时为 null：%s", b)
	}
	p := &app.PlanProgressDetail{CardID: 9007199254740993, PlanProgressView: domain.PlanProgressView{
		Status: domain.PlanProgressPartialSuccess, Total: 2, Succeeded: 1, Failed: 1,
		Items: []domain.PlanItemProgress{
			{Key: "v1", Label: "变体 1", Variant: 1, VariantLabel: "反问开场", State: domain.PlanItemSuccess, Attempts: 1},
			{Key: "v2", Label: "变体 2", State: domain.PlanItemFailed, ErrorCode: "PROVIDER_FAILED", Attempts: 1},
		}}}
	b, _ = json.Marshal(planProgressOf(p))
	want := `{"cardId":"9007199254740993","status":"partial_success","total":2,"succeeded":1,"failed":1,` +
		`"stopped":0,"running":0,"pending":0,"items":[{"key":"v1","label":"变体 1","variant":1,` +
		`"variantLabel":"反问开场","state":"success","attempts":1},{"key":"v2","label":"变体 2","state":"failed",` +
		`"errorCode":"PROVIDER_FAILED","attempts":1}]}`
	if string(b) != want {
		t.Fatalf("形状：\n%s\n期望\n%s", b, want)
	}
}
