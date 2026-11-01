//go:build integration

// 受理链路的营销片段拼接（MKT-T-21）真库测试。
//
// 营销域、目录域（约束段模板，Fresh 清了表 → 走代码兜底）、生成受理与执行器都是真的，
// 只有上游换成记录请求的假 Provider：它按 HTTPProvider.composeInputs 的规则还原发出去的提示词
// （节点 prompt 在前、快照里的附加段在后）。真实请求体落 provider_request_log 的断言在 E2E（MKT-T-30）。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	mkdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// marketingSource 与 wiring.marketingAppendixAdapter 同形（那个是未导出的）。
type marketingSource struct{ svc *marketingapp.Service }

func (m marketingSource) Appendix(ctx context.Context, _ genapp.MarketingScope, params json.RawMessage) (genapp.MarketingAppendix, error) {
	r, err := m.svc.Appendix(ctx, params)
	return genapp.MarketingAppendix{Text: r.Text, Picks: r.Picks}, err
}

// recordingProvider 记下提交给上游的工作项，并按 composeInputs 的规则还原提示词。
type recordingProvider struct {
	mu      sync.Mutex
	prompts []string
}

func (p *recordingProvider) Submit(_ context.Context, job genapp.Job) (genapp.SubmitResult, error) {
	var params map[string]json.RawMessage
	_ = json.Unmarshal(job.Params, &params)
	var own string
	_ = json.Unmarshal(params["prompt"], &own)
	var snap struct {
		PromptSuffix string `json:"promptSuffix"`
	}
	_ = json.Unmarshal(params["_snapshot"], &snap)
	parts := []string{}
	for _, s := range []string{own, snap.PromptSuffix} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	p.mu.Lock()
	p.prompts = append(p.prompts, strings.Join(parts, "\n"))
	p.mu.Unlock()
	return genapp.SubmitResult{Outcome: gendomain.SubmitCompleted, ProviderTaskID: "up-1"}, nil
}

func (p *recordingProvider) Query(context.Context, genapp.Job, string) (genapp.QueryResult, error) {
	return genapp.QueryResult{Done: true, Success: true}, nil
}

func (p *recordingProvider) CanQueryByIdempotencyKey() bool { return false }

// lengthQuoter 按 max_input_chars 的规则判长度（含 AppendixChars），其余同 fakeQuoter。
// 判定规则本身的真库测试在 catalog/repo/integration（TestPricing_长度含附加段）。
type lengthQuoter struct {
	fakeQuoter
	maxChars int
	asks     *[]genapp.QuoteAsk
}

func (q lengthQuoter) QuoteForAccept(ctx context.Context, in genapp.QuoteAsk) (genapp.QuoteAnswer, error) {
	*q.asks = append(*q.asks, in)
	if q.maxChars > 0 && utf8.RuneCountInString(in.Text)+in.AppendixChars > q.maxChars {
		return genapp.QuoteAnswer{}, httpx.Errf(errcode.InvalidArgument, "输入超过该模型上限")
	}
	return q.fakeQuoter.QuoteForAccept(ctx, in)
}

type mkEnv struct {
	svc   *genapp.Service
	r     *marketingrepo.Repo
	pool  *pgxpool.Pool
	next  func() int64
	asks  []genapp.QuoteAsk
	ids   map[string]int64
	quote *lengthQuoter
}

func setupMarketing(t *testing.T) *mkEnv {
	t.Helper()
	svc, _, ledger, pool := setup(t, 10)
	fund(t, ledger, 1000)
	gen, _ := id.New(1)
	e := &mkEnv{svc: svc, r: marketingrepo.New(pool), pool: pool, next: gen.Next, ids: map[string]int64{}}
	e.quote = &lengthQuoter{fakeQuoter: fakeQuoter{credits: 10}, asks: &e.asks}
	svc.SetQuoter(e.quote)
	msvc := marketingapp.NewService(e.r, nil, nil, marketingapp.Settings{})
	// 约束段走真实的目录域渲染：Fresh 清空了 prompt_template，用的是代码兜底。
	msvc.SetPrompts(catalogapp.New(catalogrepo.New(pool), gen.Next))
	svc.SetMarketing(marketingSource{msvc})
	for _, k := range []mkdomain.Kind{mkdomain.KindCreative, mkdomain.KindHook, mkdomain.KindStyle} {
		e.insert(t, k, "mk_"+string(k), "【"+string(k)+"】v1")
	}
	return e
}

func (e *mkEnv) snapshot(o mkdomain.Option, v int) json.RawMessage {
	b, _ := json.Marshal(mkdomain.Snapshot{Pick: mkdomain.Pick{Kind: o.Kind, Code: o.Code, Version: v},
		Name: o.Name, Prompt: o.Prompt})
	return b
}

func (e *mkEnv) option(kind mkdomain.Kind, code, prompt string) mkdomain.Option {
	return mkdomain.Option{Kind: kind, Code: code, Name: code, CategoryCode: "cat", Prompt: prompt,
		OutputTypes: []string{"video"}, CoverMediaIDs: []int64{}, Tags: []string{}, IsEnabled: true}
}

func (e *mkEnv) insert(t *testing.T, kind mkdomain.Kind, code, prompt string) {
	t.Helper()
	o := e.option(kind, code, prompt)
	row, err := e.r.InsertOption(context.Background(), e.next(), e.next(), 1, o, e.snapshot(o, 1))
	if err != nil {
		t.Fatal(err)
	}
	e.ids[code] = row.ID
}

func (e *mkEnv) accept(key, picks string) (genapp.AcceptResult, error) {
	params := `{"prompt":"画面描述：保温杯"}`
	if picks != "" {
		params = `{"prompt":"画面描述：保温杯","marketing":{"picks":` + picks + `}}`
	}
	return e.svc.Accept(context.Background(), genapp.AcceptInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ProjectID: proj,
		NodeID: "n_video_1", OperationCode: "text2video", ModelCode: "dev_video",
		IdempotencyKey: key, Count: 1, Params: json.RawMessage(params),
	})
}

func suffixOf(t *testing.T, g genapp.AcceptResult) string {
	t.Helper()
	var p struct {
		Snapshot struct {
			PromptSuffix string `json:"promptSuffix"`
		} `json:"_snapshot"`
	}
	if err := json.Unmarshal(g.Generation.ParamsSnapshot, &p); err != nil {
		t.Fatal(err)
	}
	return p.Snapshot.PromptSuffix
}

const allPicks = `[{"kind":"style","code":"mk_style","version":1},{"kind":"hook","code":"mk_hook","version":1},` +
	`{"kind":"creative","code":"mk_creative","version":1}]`

// MKT-AC-07：发给上游的提示词按出现位置 画面描述 < 创意 < Hook < 风格 < 场景约束 < 素材词；
// MKT-AC-06：节点 params 里只有 {kind, code, version}，没有片段原文。
func TestAcceptMarketing_请求体拼接顺序(t *testing.T) {
	e := setupMarketing(t)
	ctx := context.Background()
	res, err := e.accept("mk-order", allPicks)
	if err != nil {
		t.Fatal(err)
	}

	// 执行一次，拿到「发给上游」的提示词。
	p := &recordingProvider{}
	ex := genapp.NewExecutor(repo.New(e.pool), p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if out, err := ex.Execute(ctx, res.Generation.ID); err != nil || out != genapp.OutcomeDone {
		t.Fatalf("执行应完成：%v %v", out, err)
	}
	if len(p.prompts) != 1 {
		t.Fatalf("应提交 1 次，实际 %d", len(p.prompts))
	}
	sent := p.prompts[0]
	order := []string{"画面描述：保温杯", "【creative】v1", "【hook】v1", "【style】v1", "商品的外形", "水墨风格"}
	last := -1
	for _, s := range order {
		i := strings.Index(sent, s)
		if i < 0 || i <= last {
			t.Fatalf("%q 应出现且排在前一段之后：\n%s", s, sent)
		}
		last = i
	}

	// 片段原文只在快照的附加段里，用户参数（= 节点 params）里的 marketing 只有钉住的身份。
	var params map[string]json.RawMessage
	_ = json.Unmarshal(res.Generation.ParamsSnapshot, &params)
	var mk struct {
		Picks []map[string]any `json:"picks"`
	}
	if err := json.Unmarshal(params["marketing"], &mk); err != nil || len(mk.Picks) != 3 {
		t.Fatalf("params.marketing 应原样保留 3 项：%s", params["marketing"])
	}
	for _, pk := range mk.Picks {
		if len(pk) != 3 || pk["kind"] == nil || pk["code"] == nil || pk["version"] == nil {
			t.Fatalf("params.marketing.picks 只应有 kind/code/version：%v", pk)
		}
	}
	if strings.Contains(string(params["marketing"]), "】v1") {
		t.Fatalf("片段原文不该进节点参数：%s", params["marketing"])
	}
	// 报价拿到的附加段长度 = 营销段码点数（不含素材词）。
	ask := e.asks[len(e.asks)-1]
	suffix := suffixOf(t, res)
	mkPart := strings.TrimSuffix(suffix, "\n水墨风格")
	if ask.AppendixChars != utf8.RuneCountInString(mkPart) || ask.AppendixChars == 0 {
		t.Fatalf("AppendixChars 应为营销段码点数 %d，实际 %d", utf8.RuneCountInString(mkPart), ask.AppendixChars)
	}
}

// MKT-AC-08 / 11 生成侧 + MKR-10。
func TestAcceptMarketing_快照不变(t *testing.T) {
	e := setupMarketing(t)
	ctx := context.Background()

	// 无 params.marketing：附加段与改动前逐字相等（fakeInputs 的素材词原样）。
	res, err := e.accept("mk-none", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := suffixOf(t, res); got != "水墨风格" {
		t.Fatalf("通用生成的附加段应不变，得到 %q", got)
	}
	if e.asks[len(e.asks)-1].AppendixChars != 0 {
		t.Fatal("通用生成不该带附加段长度")
	}

	// 运营把创意改成 v2，再下线它。
	o := e.option(mkdomain.KindCreative, "mk_creative", "【creative】v2")
	if _, err := e.r.UpdateOption(ctx, e.ids["mk_creative"], 1, e.next(), 1, o, e.snapshot(o, 2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.r.SetOptionEnabled(ctx, e.ids["mk_creative"], false, 1); err != nil {
		t.Fatal(err)
	}

	one := func(v string) string { return `[{"kind":"creative","code":"mk_creative","version":` + v + `}]` }
	res, err = e.accept("mk-v1", one("1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := suffixOf(t, res); !strings.HasPrefix(got, "【creative】v1\n") || strings.Contains(got, "v2") {
		t.Fatalf("钉 v1 应拼旧片段（已下线照常拼）：%q", got)
	}
	res, err = e.accept("mk-v2", one("2"))
	if err != nil {
		t.Fatal(err)
	}
	if got := suffixOf(t, res); !strings.HasPrefix(got, "【creative】v2\n") {
		t.Fatalf("钉 v2 应拼新片段：%q", got)
	}

	// 一样没选：只有约束段（它不是选项的附属）。
	res, err = e.accept("mk-empty", `[]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := suffixOf(t, res); !strings.HasPrefix(got, "商品的外形") || !strings.HasSuffix(got, "\n水墨风格") {
		t.Fatalf("marketing 段存在但为空时应只追加约束段：%q", got)
	}

	// 不存在的版本 → 40001，不留任务。
	var before int
	_ = e.pool.QueryRow(ctx, `select count(*) from generation`).Scan(&before)
	_, err = e.accept("mk-missing", one("9"))
	var biz *httpx.BizError
	if !errors.As(err, &biz) || biz.Code != errcode.InvalidArgument {
		t.Fatalf("不存在的版本应 40001，得到 %v", err)
	}
	var after int
	_ = e.pool.QueryRow(ctx, `select count(*) from generation`).Scan(&after)
	if after != before {
		t.Fatalf("受理失败不该留下任务：%d → %d", before, after)
	}
}

// MKD-17：附加段使总长超 max_input_chars → 扣费前拒，账本无新行。
func TestAcceptMarketing_超长扣费前拒(t *testing.T) {
	e := setupMarketing(t)
	ctx := context.Background()
	// 用户 Text 为空（描述在 params.prompt），附加段本身就超过 20 字上限。
	e.quote.maxChars = 20
	e.svc.SetQuoter(e.quote)

	countTx := func() int {
		var n int
		_ = e.pool.QueryRow(ctx, `select count(*) from credit_transaction where payer_account_id = $1`, payer).Scan(&n)
		return n
	}
	before := countTx()
	_, err := e.accept("mk-long", allPicks)
	var biz *httpx.BizError
	if !errors.As(err, &biz) || biz.Code != errcode.InvalidArgument {
		t.Fatalf("附加段超长应扣费前 40001，得到 %v", err)
	}
	if countTx() != before {
		t.Fatal("扣费前拒绝不该在账本留下任何流水")
	}
	var gens int
	_ = e.pool.QueryRow(ctx, `select count(*) from generation`).Scan(&gens)
	if gens != 0 {
		t.Fatalf("不该留下任务，实际 %d", gens)
	}
	// 不带营销段的同一请求照常受理（附加段只来自营销）。
	if _, err := e.accept("mk-short", ""); err != nil {
		t.Fatalf("无营销段时应放行：%v", err)
	}
}
