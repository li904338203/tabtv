//go:build integration

// 预览素材的分析缓存（docs/agent/smartcut SCT-16，SCD-30）：排队去重、逐行领取（skip locked + CAS）、
// 租约接管与晚到写回作废、失败重试与封顶、进程退出放回、永久删除时连带清理。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

type analysisEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
	r    *mediarepo.Repo
	next func() int64
}

func newAnalysisEnv(t *testing.T) analysisEnv {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return analysisEnv{t: t, pool: pool, r: mediarepo.New(pool), next: gen.Next}
}

func (e analysisEnv) media(ws int64, contentType, status string) int64 {
	e.t.Helper()
	mid := e.next()
	if _, err := e.pool.Exec(context.Background(), `insert into media_object (id, workspace_id, bucket, object_key,
		content_type, size_bytes, purpose, created_by_user_id, status, deleted_at)
		values ($1,$2,'b',$3,$4,10,'upload_reference',1,$5::text, case when $5::text = 'deleted' then now() end)`,
		mid, ws, fmt.Sprintf("k/%d", mid), contentType, status); err != nil {
		e.t.Fatal(err)
	}
	return mid
}

func (e analysisEnv) enqueue(ws int64, keys ...mediarepo.AnalysisKey) int {
	e.t.Helper()
	n, err := e.r.EnqueueAnalyses(context.Background(), e.next, ws, keys)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func key(mediaID int64, kind string) mediarepo.AnalysisKey {
	return mediarepo.AnalysisKey{MediaID: mediaID, Kind: kind, Engine: domain.AnalysisEngine(kind)}
}

func (e analysisEnv) count(where string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `select count(*) from media_analysis where `+where, args...).
		Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

var scannerKinds = domain.ScannerKinds

// 重复排队不产生新行；只排本工作区、未删除的媒体；failed 之后可以重新排队。
func TestAnalysis_排队去重与归属(t *testing.T) {
	e := newAnalysisEnv(t)
	ctx := context.Background()
	const ws = int64(7)
	video := e.media(ws, "video/mp4", "active")
	other := e.media(8, "video/mp4", "active")
	gone := e.media(ws, "video/mp4", "deleted")

	if n := e.enqueue(ws, key(video, "preview_proxy"), key(video, "peaks"), key(other, "peaks"),
		key(gone, "peaks")); n != 2 {
		t.Fatalf("只排本工作区、未删除的：%d", n)
	}
	if n := e.enqueue(ws, key(video, "preview_proxy"), key(video, "peaks")); n != 0 {
		t.Fatalf("排队中的不重复排：%d", n)
	}
	// 引擎版本不同是另一个缓存键（算法升级后要重做）。
	if n := e.enqueue(ws, mediarepo.AnalysisKey{MediaID: video, Kind: "peaks", Engine: "peaks-v2"}); n != 1 {
		t.Fatalf("新引擎版本应排进：%d", n)
	}

	// 先排的先领（按创建时间）：领到的是 v1 那行。
	job, err := e.r.ClaimAnalysis(ctx, []string{"peaks"}, time.Minute, 3)
	if err != nil || job.Engine != domain.AnalysisEngine("peaks") {
		t.Fatalf("领取：%+v %v", job, err)
	}
	if ok, err := e.r.CompleteAnalysis(ctx, job.ID, job.Attempts, json.RawMessage(`{"intervalMs":10,"values":""}`), 0); err != nil || !ok {
		t.Fatalf("完成：%v %v", ok, err)
	}
	if n := e.enqueue(ws, key(video, "peaks")); n != 0 {
		t.Fatalf("成功的不重复排：%d", n)
	}

	// 判失败之后可以重新排队（唯一索引不含 failed）。
	job2, err := e.r.ClaimAnalysis(ctx, []string{"preview_proxy"}, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := e.r.FailAnalysis(ctx, job2.ID, job2.Attempts, "坏文件", time.Minute, 3, true); err != nil || st != "failed" {
		t.Fatalf("永久失败直接判 failed：%q %v", st, err)
	}
	if n := e.enqueue(ws, key(video, "preview_proxy")); n != 1 {
		t.Fatalf("failed 不挡重新排队：%d", n)
	}
	if e.count(`media_object_id = $1 and kind = 'preview_proxy'`, video) != 2 {
		t.Fatal("失败的那行保留（排障要看原因），新排一行")
	}

	rows, err := e.r.ListAnalyses(ctx, ws, video)
	if err != nil || len(rows) != 4 {
		t.Fatalf("列出本工作区这份媒体的全部分析：%d %v", len(rows), err)
	}
	if rows, _ := e.r.ListAnalyses(ctx, 8, video); len(rows) != 0 {
		t.Fatal("别的工作区查不到")
	}
}

// 多个扫描器同时领取同一行，只有一个领到（真并发；先预热连接池、跑多轮）。
func TestAnalysis_并发领取只有一个成功(t *testing.T) {
	e := newAnalysisEnv(t)
	ctx := context.Background()
	const ws, workers, rounds = int64(7), 8, 20

	// 预热：先让池子里有足够的连接，否则「同时」其实是排着队建连接，抢不起来。
	var warm sync.WaitGroup
	for i := 0; i < workers; i++ {
		warm.Add(1)
		go func() {
			defer warm.Done()
			_, _ = e.pool.Exec(ctx, `select pg_sleep(0.05)`)
		}()
	}
	warm.Wait()

	for round := 0; round < rounds; round++ {
		video := e.media(ws, "video/mp4", "active")
		if e.enqueue(ws, key(video, "preview_proxy")) != 1 {
			t.Fatal("排队失败")
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		got := 0
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := e.r.ClaimAnalysis(ctx, scannerKinds, time.Minute, 3)
				if err == nil {
					mu.Lock()
					got++
					mu.Unlock()
				} else if !errors.Is(err, mediarepo.ErrNoAnalysis) {
					t.Error(err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got != 1 {
			t.Fatalf("第 %d 轮：%d 个扫描器领到了同一行", round, got)
		}
		if e.count(`media_object_id = $1 and attempts = 1 and status = 'running'`, video) != 1 {
			t.Fatalf("第 %d 轮：attempts 应只加一次", round)
		}
	}
}

// 租约过期后另一个实例接管；原执行者的续期与写回都不再成立，接管者的写回生效。
func TestAnalysis_租约过期被接管_晚到的写回作废(t *testing.T) {
	e := newAnalysisEnv(t)
	ctx := context.Background()
	video := e.media(7, "video/mp4", "active")
	e.enqueue(7, key(video, "filmstrip"))

	a, err := e.r.ClaimAnalysis(ctx, scannerKinds, 300*time.Millisecond, 3)
	if err != nil {
		t.Fatal(err)
	}
	// 租约还在：别人领不到。
	if _, err := e.r.ClaimAnalysis(ctx, scannerKinds, time.Minute, 3); !errors.Is(err, mediarepo.ErrNoAnalysis) {
		t.Fatalf("租约内不该被领走：%v", err)
	}
	// 续期能把租约往后推：活着的执行者不会被接管。
	if ok, err := e.r.RenewAnalysisLease(ctx, a.ID, a.Attempts, 300*time.Millisecond); err != nil || !ok {
		t.Fatalf("续期：%v %v", ok, err)
	}
	var b mediarepo.Analysis
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err = e.r.ClaimAnalysis(ctx, scannerKinds, time.Minute, 3)
		if err == nil {
			break
		}
		if !errors.Is(err, mediarepo.ErrNoAnalysis) || time.Now().After(deadline) {
			t.Fatalf("租约过期后应能被接管：%v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if b.ID != a.ID || b.Attempts != 2 {
		t.Fatalf("接管的是同一行、attempts 2：%+v", b)
	}
	if ok, _ := e.r.RenewAnalysisLease(ctx, a.ID, a.Attempts, time.Minute); ok {
		t.Fatal("原执行者的续期不再成立")
	}
	if ok, _ := e.r.CompleteAnalysis(ctx, a.ID, a.Attempts, json.RawMessage(`{"by":"a"}`), 0); ok {
		t.Fatal("原执行者晚到的写回应作废")
	}
	if st, _ := e.r.FailAnalysis(ctx, a.ID, a.Attempts, "a 失败", time.Minute, 3, false); st != "" {
		t.Fatal("原执行者晚到的失败也不记")
	}
	if ok, err := e.r.CompleteAnalysis(ctx, b.ID, b.Attempts, json.RawMessage(`{"by":"b"}`), 0); err != nil || !ok {
		t.Fatalf("接管者写回：%v %v", ok, err)
	}
	got, _ := e.r.GetAnalysis(ctx, a.ID)
	if got.Status != "success" || string(got.Result) != `{"by": "b"}` || got.LeaseUntil != nil {
		t.Fatalf("结果是接管者的：%+v %s", got, got.Result)
	}
}

// 失败未满次数：放回排队并退避（退避期内领不到）；满 3 次判 failed 带原因。
func TestAnalysis_失败重试与封顶(t *testing.T) {
	e := newAnalysisEnv(t)
	ctx := context.Background()
	video := e.media(7, "video/mp4", "active")
	e.enqueue(7, key(video, "peaks"))

	claim := func() mediarepo.Analysis {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			j, err := e.r.ClaimAnalysis(ctx, scannerKinds, time.Minute, 3)
			if err == nil {
				return j
			}
			if !errors.Is(err, mediarepo.ErrNoAnalysis) || time.Now().After(deadline) {
				t.Fatalf("领取：%v", err)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	for attempt := 1; attempt <= 3; attempt++ {
		j := claim()
		if j.Attempts != attempt {
			t.Fatalf("第 %d 次领取 attempts=%d", attempt, j.Attempts)
		}
		st, err := e.r.FailAnalysis(ctx, j.ID, j.Attempts, fmt.Sprintf("第 %d 次失败", attempt), 200*time.Millisecond, 3, false)
		if err != nil {
			t.Fatal(err)
		}
		want := "queued"
		if attempt == 3 {
			want = "failed"
		}
		if st != want {
			t.Fatalf("第 %d 次失败后：%q，应为 %q", attempt, st, want)
		}
		if attempt < 3 {
			// 退避期内领不到（立即重试多半撞上同一个临时故障）。
			if _, err := e.r.ClaimAnalysis(ctx, scannerKinds, time.Minute, 3); !errors.Is(err, mediarepo.ErrNoAnalysis) {
				t.Fatalf("退避期内不该被领：%v", err)
			}
		}
	}
	rows, _ := e.r.ListAnalyses(ctx, 7, video)
	if len(rows) != 1 || rows[0].Status != "failed" || rows[0].ErrorMessage != "第 3 次失败" {
		t.Fatalf("满 3 次判 failed 并带原因：%+v", rows)
	}
}

// 试满次数后又中途断掉（进程被杀）：领取不再拿它，ReapAnalyses 判失败 —— 不封顶的话坏文件会无限循环。
func TestAnalysis_中断满次数判失败_退出时放回(t *testing.T) {
	e := newAnalysisEnv(t)
	ctx := context.Background()
	video := e.media(7, "video/mp4", "active")
	audio := e.media(7, "audio/mpeg", "active")
	e.enqueue(7, key(video, "preview_proxy"), key(audio, "beats"))

	if _, err := e.pool.Exec(ctx, `update media_analysis set status = 'running', attempts = 3,
		lease_until = now() - interval '1 second' where media_object_id = $1`, video); err != nil {
		t.Fatal(err)
	}
	j, err := e.r.ClaimAnalysis(ctx, scannerKinds, time.Minute, 3)
	if err != nil || j.MediaObjectID != audio {
		t.Fatalf("试满次数的不再领取，只领到另一行：%+v %v", j, err)
	}
	n, err := e.r.ReapAnalyses(ctx, scannerKinds, 3)
	if err != nil || n != 1 {
		t.Fatalf("判失败一行：%d %v", n, err)
	}
	rows, _ := e.r.ListAnalyses(ctx, 7, video)
	if rows[0].Status != "failed" || rows[0].ErrorMessage == "" || rows[0].LeaseUntil != nil {
		t.Fatalf("带原因判失败：%+v", rows[0])
	}

	// 进程退出：放回排队、退回这一次的计数，马上能被别人领走。
	if err := e.r.ReleaseAnalysis(ctx, j.ID, j.Attempts); err != nil {
		t.Fatal(err)
	}
	again, err := e.r.ClaimAnalysis(ctx, scannerKinds, time.Minute, 3)
	if err != nil || again.ID != j.ID || again.Attempts != 1 {
		t.Fatalf("放回后重新领取，attempts 不累加：%+v %v", again, err)
	}
	// 只领扫描器的种类：识别结果由识别任务写，扫描器领走会被当成不认识的种类判失败。
	if _, err := e.pool.Exec(ctx, `insert into media_analysis (id, workspace_id, media_object_id, kind, engine)
		values ($1, 7, $2, 'asr', 'volc')`, e.next(), audio); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.ClaimAnalysis(ctx, scannerKinds, time.Minute, 3); !errors.Is(err, mediarepo.ErrNoAnalysis) {
		t.Fatalf("asr 不归扫描器：%v", err)
	}
}

// 永久删除媒体时连带删掉以它为源、或以它为产物的分析行。
func TestAnalysis_永久删除连带清理(t *testing.T) {
	e := newAnalysisEnv(t)
	ctx := context.Background()
	video := e.media(7, "video/mp4", "active")
	proxy := e.media(7, "video/mp4", "active")
	keep := e.media(7, "video/mp4", "active")
	e.enqueue(7, key(video, "preview_proxy"), key(video, "peaks"), key(keep, "peaks"))
	j, err := e.r.ClaimAnalysis(ctx, []string{"preview_proxy"}, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.r.CompleteAnalysis(ctx, j.ID, j.Attempts, json.RawMessage(`{"width":854,"height":480}`), proxy); !ok {
		t.Fatal("完成失败")
	}
	// 先删产物（回收器同一批里顺序不定）：指着它的分析行一起删，剪辑台不会拿到一个不存在的预览版。
	if err := e.r.DeleteRow(ctx, proxy); err != nil {
		t.Fatal(err)
	}
	if e.count(`media_object_id = $1 and kind = 'preview_proxy'`, video) != 0 ||
		e.count(`media_object_id = $1 and kind = 'peaks'`, video) != 1 {
		t.Fatal("只删指着这份产物的那行")
	}
	if err := e.r.DeleteRow(ctx, video); err != nil {
		t.Fatal(err)
	}
	if e.count(`media_object_id = $1`, video) != 0 || e.count(`media_object_id = $1`, keep) != 1 {
		t.Fatal("删源时连带删掉它的全部分析行，别人的不动")
	}
}
