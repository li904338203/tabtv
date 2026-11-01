//go:build integration

// 商品库 / 角色库会话与旧会话在执行侧的兼容（MKT-T-37；dev/05 MKD-25、MKD-33）。
//
// 设计要求快照形状不变、下游（选图 / view_media / generate）一行不改：这里用真 agent_session 行
// 分别造「商品库会话」（新形状：productId、角色快照带 characterId 与参考音频）与「旧会话」
// （20 张 products、上传的人物图），走同一套 view_media / generate，钉住两件事：
//   - 旧会话照常（MKT-AC-44）：view_media #20、按 #20 生成；
//   - 新会话生成时角色用主图、**参考音频不进受理输入**（MKT-AC-45 接口部分）。
package integration

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
)

// seedSceneSession 建一个执行态的营销会话（scene_input 原样写库）。
func seedSceneSession(t *testing.T, e *paidEnv, si string) repo.Session {
	t.Helper()
	s, err := e.r.Create(context.Background(), e.pool, repo.Session{ID: e.next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payer, CreatedByUserID: user, State: domain.StateExecuting, ApprovedBudget: 500,
		Prefs: json.RawMessage(`{"reasoning":{"model":"m-agent"}}`), Scene: domain.SceneMarketing,
		SceneInput: json.RawMessage(si)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// libImages 记下 view_media 取的是哪张图。
type libImages struct{ got []int64 }

func (l *libImages) ReasoningImageLimit(context.Context, string) (int, error) { return 4, nil }
func (l *libImages) InlineImage(_ context.Context, _ int64, id int64) (string, error) {
	l.got = append(l.got, id)
	return "data:image/png;base64,AAAA", nil
}

func TestMarketingSession_旧会话兼容(t *testing.T) {
	e, probe := newMarketingEnv(t, 100)
	ctx := context.Background()
	products := make([]string, 20)
	for i := range products {
		products[i] = strconv.Itoa(7001 + i)
	}
	pj, _ := json.Marshal(products)
	// 旧形状：没有 productId / description，角色是上传的人物图（kind=media、没有 characterId）。
	s := seedSceneSession(t, e, `{"products":`+string(pj)+`,"product":{"name":"保温杯","sellingPoints":"24 小时保温"},`+
		`"characters":[{"kind":"media","id":"7301"}],"sourceVideo":null,`+
		`"picks":{"creative":null,"hook":null,"style":null},"pins":[]}`)

	// view_media #20：取的是第 20 张。
	imgs := &libImages{}
	vm := agenttools.ViewMedia{Sessions: e.r, Images: imgs}
	rc := e.step(t, s, "view_media")
	if _, err := vm.Run(ctx, rc, json.RawMessage(`{"slot":"product","index":20}`)); err != nil {
		t.Fatal(err)
	}
	if len(imgs.got) != 1 || imgs.got[0] != 7020 {
		t.Fatalf("view_media #20 应取第 20 张：%v", imgs.got)
	}

	// 规划选 #20 并批准 → 生成：受理收到 #20 + 上传的人物图。
	limit := 3
	it := videoItem("v1", []string{"7020"}, []int{20})
	it.ProductTotal, it.ImageLimit, it.Picks = 20, &limit, nil
	planCard(t, e, s, []domain.PlanItem{it}, "approve")
	rc = e.step(t, s, "generate")
	rc.Scene = domain.SceneMarketing
	if _, err := e.tool(domain.ToolGenerate).Run(ctx, rc, json.RawMessage(`{"planItemKey":"v1","wait":false}`)); err != nil {
		t.Fatal(err)
	}
	if got := declaredIDs(t, probe, 0); !equalStrings(got, []string{"7020", "7301"}) {
		t.Fatalf("旧会话生成输入应是 #20 + 人物图：%v", got)
	}
	// 上下文摘要按旧格式（没有描述行、上传的人物图）—— 摘要的逐字断言在 marketing/domain 的 fixture。
	parsed, err := agentapp.ParseSceneInput(s.SceneInput)
	if err != nil || parsed.ProductID != "" || parsed.Characters[0].CharacterID != "" {
		t.Fatalf("旧会话解析出的 scene_input 不应凭空多出商品库字段：%+v %v", parsed, err)
	}
}

func TestMarketingSession_商品库会话生成不发参考音频(t *testing.T) {
	e, probe := newMarketingEnv(t, 100)
	ctx := context.Background()
	// 新形状：商品 P（3 张，展开成 products）+ 角色库角色（主图 8401、参考音频 8499）+ 脚本资产角色以外的一切照旧。
	s := seedSceneSession(t, e, `{"productId":"8100","productVersion":2,"products":["8001","8002","8003"],`+
		`"product":{"name":"保温杯","sellingPoints":"24 小时保温","description":"双层真空"},`+
		`"characters":[{"kind":"media","id":"8401","characterId":"8400","name":"柚子君","appearance":"圆脸",`+
		`"voiceText":"少年音","voiceMediaId":"8499"}],"sourceVideo":null,`+
		`"picks":{"creative":null,"hook":null,"style":null},"pins":[]}`)
	it := videoItem("v1", []string{"8003", "8001"}, []int{3, 1})
	it.Picks = nil
	planCard(t, e, s, []domain.PlanItem{it}, "approve")
	rc := e.step(t, s, "generate")
	rc.Scene = domain.SceneMarketing
	if _, err := e.tool(domain.ToolGenerate).Run(ctx, rc, json.RawMessage(`{"planItemKey":"v1","wait":false}`)); err != nil {
		t.Fatal(err)
	}
	if got := declaredIDs(t, probe, 0); !equalStrings(got, []string{"8003", "8001", "8401"}) {
		t.Fatalf("输入 = 条目选的商品图 + 角色主图：%v", got)
	}
	if strings.Contains(string(probe.seen[0]), "8499") {
		t.Fatalf("参考音频不得进入受理输入（MKD-33）：%s", probe.seen[0])
	}
	// view_media 序号对应商品 P 的图片顺序（MKT-AC-47）。
	imgs := &libImages{}
	vm := agenttools.ViewMedia{Sessions: e.r, Images: imgs}
	if _, err := vm.Run(ctx, e.step(t, s, "view_media"), json.RawMessage(`{"slot":"product","index":2}`)); err != nil {
		t.Fatal(err)
	}
	if len(imgs.got) != 1 || imgs.got[0] != 8002 {
		t.Fatalf("view_media #2 = 商品 P 的第 2 张：%v", imgs.got)
	}
}

// declaredIDs 取第 i 次受理收到的输入引用 ID（按 order）。
func declaredIDs(t *testing.T, p *declaredProbe, i int) []string {
	t.Helper()
	if len(p.seen) <= i {
		t.Fatalf("受理次数不足：%d", len(p.seen))
	}
	var arr []map[string]any
	_ = json.Unmarshal(p.seen[i], &arr)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		out = append(out, x["referenceId"].(string))
	}
	return out
}
