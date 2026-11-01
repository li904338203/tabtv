//go:build integration

// 输入区间（docs/agent/smartcut SCT-32，SCD-34）的受理链路：真实装配（画布域解析与校验区间、目录域按 per_second
// 报价、账本扣费），只把模型配置与素材按需写进库。
//
// ⚠️ 用真实装配而不是本包其它用例的替身：要守的恰恰是「区间从声明一路走到报价与快照」，
// 替身会把中间每一跳都变成测试作者的假设。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

type rangeEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	next   func() int64
	gen    *genapp.Service
	ledger *billingrepo.Repo
	pid    int64
}

func newRangeEnv(t *testing.T) rangeEnv {
	t.Helper()
	pool := testsupport.Fresh(t)
	g, _ := id.New(0)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	core, err := wiring.BuildCore(wiring.CoreInput{Cfg: config.Config{},
		Deps: &bootstrap.Deps{DB: &db.Pool{Pool: pool}}, NextID: g.Next, Logger: logger,
		Audit:   audit.New(pool, g.Next, logger),
		Media:   mediaapp.New(mediarepo.New(pool), nil, g.Next),
		Catalog: catalogapp.New(catalogrepo.New(pool), g.Next)})
	if err != nil {
		t.Fatal(err)
	}
	e := rangeEnv{t: t, pool: pool, next: g.Next, gen: core.Gen, ledger: billingrepo.New(pool, g.Next)}
	e.exec(`insert into project (id, workspace_id, created_by_user_id, name, canvas_data)
		values ($1,$2,$3,'项目','{"nodes":[],"edges":[]}'::jsonb)`, proj, ws, user)
	e.pid = proj
	// 去字幕：按秒计价，每秒 2 积分；只收一段视频（能力矩阵必配）。
	e.exec(`insert into api_protocol (code, name, is_async) values ('generic_async','通用异步',true)
		on conflict (code) do nothing`)
	pvd, mdl, op := g.Next(), g.Next(), g.Next()
	e.exec(`insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
		values ($1,'p-erase','去字幕上游','http','https://upstream.example.com','none','enabled')`, pvd)
	e.exec(`insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol,
		submit_path, query_path, est_seconds, status) values ($1,'erase-m','去字幕',$2,'video','up','generic_async',
		'/submit','/query/{taskId}',60,'enabled')`, mdl, pvd)
	e.exec(`insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,'video_desubtitle','去字幕','video','video','video','enabled')`, op)
	e.exec(`insert into model_operation (id, model_id, operation_id, is_enabled) values ($1,$2,$3,true)`,
		g.Next(), mdl, op)
	e.exec(`insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		values ($1,$2,$3,'video',1,1)`, g.Next(), mdl, op)
	e.exec(`insert into pricing (id, operation_id, model_id, credits, unit, status)
		values ($1,$2,$3,2,'per_second','enabled')`, g.Next(), op, mdl)
	fund(t, e.ledger, 100)
	return e
}

func (e rangeEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s：%v", sql, err)
	}
}

// media 插一份本工作区的素材（durationMs 为 0 表示时长未知）。
func (e rangeEnv) media(contentType string, durationMs int) int64 {
	e.t.Helper()
	mid := e.next()
	e.exec(`insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes, purpose,
		created_by_user_id, duration_ms) values ($1,$2,'b',$3,$4,10,'upload_reference',$5,nullif($6,0))`,
		mid, ws, fmt.Sprintf("k/%d", mid), contentType, user, durationMs)
	return mid
}

func (e rangeEnv) input(mediaID int64, rangeMs string) genapp.AcceptInput {
	decl := fmt.Sprintf(`[{"kind":"reference","order":1,"referenceId":"%d","rangeMs":%s}]`, mediaID, rangeMs)
	return genapp.AcceptInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ProjectID: e.pid, NodeID: "n_sc",
		OperationCode: "video_desubtitle", ModelCode: "erase-m",
		IdempotencyKey: fmt.Sprintf("erase-%d-%s", mediaID, strings.NewReplacer("[", "", "]", "", ",", "-").Replace(rangeMs)),
		// ⚠️ 客户端报 60 秒：带区间时不收这个数，按区间算。
		DurationSec: 60, Params: json.RawMessage(`{}`), DeclaredInputs: json.RawMessage(decl),
	}
}

func rangeBizCode(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

// 60 秒素材取 [2000, 5000]：报价按 3 秒（2 × 3 = 6 积分），不按客户端报的 60 秒；快照里有区间；扣的是 6。
// 重试照样只用那一段、按那一段计价。
func TestAccept_带区间只按区间计价并进快照(t *testing.T) {
	e := newRangeEnv(t)
	ctx := context.Background()
	video := e.media("video/mp4", 60000)

	pre, err := e.gen.Preview(ctx, e.input(video, "[2000,5000]"))
	if err != nil || pre.QuoteErr != nil {
		t.Fatalf("预览: %v %v", err, pre.QuoteErr)
	}
	if pre.Quote.Credits != 6 {
		t.Fatalf("报价应按区间 3 秒 = 6 积分，实际 %d", pre.Quote.Credits)
	}

	res, err := e.gen.Accept(ctx, e.input(video, "[2000,5000]"))
	if err != nil {
		t.Fatal(err)
	}
	g := res.Generation
	if g.CreditsCost != 6 {
		t.Fatalf("扣费应为 6，实际 %d", g.CreditsCost)
	}
	if bal, _ := e.ledger.Balance(ctx, payer); bal != 94 {
		t.Fatalf("余额应剩 94，实际 %d", bal)
	}
	var snap []map[string]any
	if err := json.Unmarshal(g.InputsSnapshot, &snap); err != nil || len(snap) != 1 {
		t.Fatalf("输入快照: %s %v", g.InputsSnapshot, err)
	}
	r, _ := snap[0]["rangeMs"].([]any)
	if len(r) != 2 || r[0] != float64(2000) || r[1] != float64(5000) || snap[0]["mediaKind"] != "video" {
		t.Fatalf("区间要进快照: %s", g.InputsSnapshot)
	}

	// 重试（还原声明形态 → 重新受理）：区间带回去，仍按 3 秒计价。
	e.exec(`update generation set status = 'failed' where id = $1`, g.ID)
	retry, err := e.gen.Retry(ctx, ws, payer, user, g.ID, "erase-retry-1")
	if err != nil {
		t.Fatal(err)
	}
	var again []struct {
		RangeMs []int `json:"rangeMs"`
	}
	_ = json.Unmarshal(retry.Generation.InputsSnapshot, &again)
	if retry.Generation.CreditsCost != 6 || len(again) != 1 || len(again[0].RangeMs) != 2 ||
		again[0].RangeMs[0] != 2000 || again[0].RangeMs[1] != 5000 {
		t.Fatalf("重试要保留区间并按区间计价: cost=%d %s", retry.Generation.CreditsCost, retry.Generation.InputsSnapshot)
	}

	// 向上取整到秒：[0, 2100] 按 3 秒算。
	pre2, err := e.gen.Preview(ctx, e.input(video, "[0,2100]"))
	if err != nil || pre2.Quote.Credits != 6 {
		t.Fatalf("2.1 秒应向上取整为 3 秒（6 积分）: %v %+v", err, pre2.Quote)
	}
}

// 区间不合法：在扣费之前拦下（40001 并指名第几个输入），不留任务、不扣钱。
func TestAccept_区间不合法时不扣费(t *testing.T) {
	e := newRangeEnv(t)
	ctx := context.Background()
	video := e.media("video/mp4", 60000)
	image := e.media("image/png", 0)
	cases := []struct {
		name    string
		media   int64
		rangeMs string
		msg     string
	}{
		{"越过素材时长", video, "[2000,61000]", "超出了素材时长"},
		{"颠倒", video, "[5000,2000]", "起点要小于终点"},
		{"图片", image, "[0,1000]", "只对视频或音频有效"},
	}
	for _, c := range cases {
		_, err := e.gen.Accept(ctx, e.input(c.media, c.rangeMs))
		if rangeBizCode(err) != errcode.InvalidArgument || !strings.Contains(err.Error(), c.msg) ||
			!strings.Contains(err.Error(), "第 1 个输入") {
			t.Errorf("%s：应 40001 并说清原因，实际 %v", c.name, err)
		}
	}
	var n int
	if err := e.pool.QueryRow(ctx, `select count(*) from generation`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("拦下的请求不该留任务: %d %v", n, err)
	}
	if bal, _ := e.ledger.Balance(ctx, payer); bal != 100 {
		t.Fatalf("不该扣费，余额 %d", bal)
	}
}

// 时长未知的素材：只校验形状，照常受理并按区间计价（拒绝等于把探测不出时长的素材整个挡在门外）。
func TestAccept_时长未知时只校验区间形状(t *testing.T) {
	e := newRangeEnv(t)
	video := e.media("video/mp4", 0)
	res, err := e.gen.Accept(context.Background(), e.input(video, "[2000,5000]"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Generation.Status != gendomain.StatusQueued || res.Generation.CreditsCost != 6 {
		t.Fatalf("应照常受理并按区间计价: %+v", res.Generation)
	}
}
