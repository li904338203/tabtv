//go:build integration

// 导出（docs/agent/smartcut SCT-15）：免费、按用户限流（真并发）、幂等、规格、预检「片段还在生成」、
// 领取不改死线与原子并发；以及 Worker：取消即停、绑定节点不登记作品、导出后自检（真 ffmpeg）。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/timeline/typeset"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type zeroQuote struct{}

func (zeroQuote) QuoteRender(context.Context, int, int64) (int, error) { return 0, nil }

const renderUser = int64(501)

func renderSetup(t *testing.T, limits timelineapp.RenderLimits) env {
	t.Helper()
	e := setup(t)
	e.svc.SetBilling(e.pool, billingrepo.New(e.pool, e.next))
	e.svc.SetNextID(e.next)
	e.svc.SetRenderQuoter(zeroQuote{})
	e.svc.SetFFmpegAvailable(true)
	e.svc.SetRenderLimits(limits)
	return e
}

// timelineWithClip 建一条 9:16 时间线，画面轨放一段 4 秒视频（素材 101）。
func (e env) timelineWithClip(t *testing.T) timelineapp.View {
	t.Helper()
	v, err := e.svc.Create(e.ctx, timelineapp.CreateInput{WorkspaceID: ws, ProjectID: 777, AspectRatio: "9:16"})
	if err != nil {
		t.Fatal(err)
	}
	return e.add(t, v, domain.TrackVideo, 101)
}

func (e env) start(v timelineapp.View, key string) (timelinerepo.RenderJob, error) {
	j, _, err := e.svc.StartRender(e.ctx, timelineapp.RenderInput{WorkspaceID: ws, PayerAccountID: 1, UserID: renderUser,
		TimelineID: v.Timeline.ID, IdempotencyKey: key})
	return j, err
}

func codeOf(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

// SC-AC-20：导出免费（余额与流水不变）；进行中再发起一个回 42902 并带重试时间；超过每小时上限回 42901。
func TestRender_免费且按用户限流(t *testing.T) {
	e := renderSetup(t, timelineapp.RenderLimits{UserConcurrency: 1, UserPerHour: 3})
	v := e.timelineWithClip(t)
	a, err := e.start(v, "k1")
	if err != nil || a.Status != domain.RenderQueued {
		t.Fatalf("第一个受理：%v（%v）", err, errors.Unwrap(err))
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `select count(*) from credit_transaction`).Scan(&n)
	if n != 0 || a.CreditsCost != 0 {
		t.Fatalf("导出免费：没有积分流水（%d）、0 积分（%d）", n, a.CreditsCost)
	}
	_, err = e.start(v, "k2")
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.ConcurrencyLimited || be.Details["retry_after_seconds"] == nil {
		t.Fatalf("进行中再发起回 42902 并带重试时间：%v", err)
	}
	// 跑完了（直接改状态）再发起：并发限制放开，但每小时上限照算。
	finish := func() {
		if _, err := e.pool.Exec(e.ctx, `update render_job set status = 'success' where status in ('queued','running')`); err != nil {
			t.Fatal(err)
		}
	}
	finish()
	if _, err := e.start(v, "k3"); err != nil {
		t.Fatalf("上一个结束后可以再导出：%v", err)
	}
	finish()
	if _, err := e.start(v, "k4"); err != nil {
		t.Fatalf("第三次：%v", err)
	}
	finish()
	_, err = e.start(v, "k5")
	if !errors.As(err, &be) || be.Code != errcode.RateLimited {
		t.Fatalf("超过每小时上限回 42901：%v", err)
	}
	if secs, _ := be.Details["retry_after_seconds"].(int); secs < 3000 || secs > 3601 {
		t.Fatalf("重试时间约一小时：%v", be.Details)
	}
	// 同一把键重复提交不计入限流：已到上限照样返回原来那一条（SC-AC-22）。
	if again, err := e.start(v, "k1"); err != nil || again.ID != a.ID {
		t.Fatalf("同键返回原任务：%v %v", again.ID, err)
	}
}

// 真并发：同一用户 8 个不同的键同时受理，只成功一个（用户锁 + 锁内计数）。先预热连接池、跑多轮。
func TestRender_用户限流真并发(t *testing.T) {
	e := renderSetup(t, timelineapp.RenderLimits{UserConcurrency: 1, UserPerHour: 1000})
	v := e.timelineWithClip(t)
	var warm sync.WaitGroup
	for i := 0; i < 8; i++ {
		warm.Add(1)
		go func() { defer warm.Done(); _, _ = e.pool.Exec(e.ctx, `select pg_sleep(0.05)`) }()
	}
	warm.Wait()
	for round := 0; round < 10; round++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		var ok atomic.Int32
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				if _, err := e.start(v, ""); err == nil {
					ok.Add(1)
				} else if codeOf(err) != errcode.ConcurrencyLimited {
					t.Error(err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if ok.Load() != 1 {
			t.Fatalf("第 %d 轮：%d 个受理成功（同时只能 1 个）", round, ok.Load())
		}
		if _, err := e.pool.Exec(e.ctx, `update render_job set status = 'success' where status = 'queued'`); err != nil {
			t.Fatal(err)
		}
	}
}

// SC-AC-22：同一把键连发 / 并发都只建一个任务，返回同一个 ID。
func TestRender_幂等键(t *testing.T) {
	e := renderSetup(t, timelineapp.RenderLimits{})
	v := e.timelineWithClip(t)
	var wg sync.WaitGroup
	ids := make(chan int64, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if j, err := e.start(v, "same"); err == nil {
				ids <- j.ID
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[int64]bool{}
	for id := range ids {
		seen[id] = true
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `select count(*) from render_job where idempotency_key = 'same'`).Scan(&n)
	if len(seen) != 1 || n != 1 {
		t.Fatalf("只建一个任务：%v 个 ID、%d 行", len(seen), n)
	}
}

// 规格冻结（SC-AC-21 的受理侧）：9:16 的 720p、25fps → 720×1280、25；预检「片段还在生成」指出第几段。
func TestRender_规格与片段还在生成(t *testing.T) {
	e := renderSetup(t, timelineapp.RenderLimits{})
	v := e.timelineWithClip(t)
	j, _, err := e.svc.StartRender(e.ctx, timelineapp.RenderInput{WorkspaceID: ws, PayerAccountID: 1, UserID: renderUser,
		TimelineID: v.Timeline.ID, Resolution: "720p", FPS: 25})
	if err != nil {
		t.Fatal(err)
	}
	var snap domain.RenderSnapshot
	_ = json.Unmarshal(j.Snapshot, &snap)
	var spec map[string]any
	_ = json.Unmarshal(j.Spec, &spec)
	if snap.Width != 720 || snap.Height != 1280 || snap.FPS != 25 || spec["resolution"] != "720p" || spec["width"] != float64(720) {
		t.Fatalf("规格：%dx%d@%d %v", snap.Width, snap.Height, snap.FPS, spec)
	}
	if _, _, err := e.svc.StartRender(e.ctx, timelineapp.RenderInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		Resolution: "4k"}); codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("4K 不支持：%v", err)
	}
	clip := trackOf(t, v, domain.TrackVideo).Clips[0]
	if _, err := e.pool.Exec(e.ctx, `update timeline_clip set pending_generation_id = 1 where id = $1`, clip.ID); err != nil {
		t.Fatal(err)
	}
	_, pre, err := e.svc.StartRender(e.ctx, timelineapp.RenderInput{WorkspaceID: ws, TimelineID: v.Timeline.ID, DryRun: true})
	if err != nil || len(pre.Issues) != 1 || pre.Issues[0].Code != domain.PrecheckClipGenerating || pre.Issues[0].ClipIdx != 1 {
		t.Fatalf("片段还在生成要拦下并指出第 1 段：%+v %v", pre.Issues, err)
	}
}

// SC-AC-25 与 SCB-10：领取不改受理时算好的死线；全站并发上限的「数」与「领」原子（两个任务、上限 1、同时领 → 一个领到）。
func TestRender_领取不改死线_并发上限原子(t *testing.T) {
	e := renderSetup(t, timelineapp.RenderLimits{})
	v := e.timelineWithClip(t)
	r := timelinerepo.New(e.pool, e.next)
	a, _ := e.start(v, "a")
	b, _ := e.start(v, "b")
	var before time.Time
	_ = e.pool.QueryRow(e.ctx, `select deadline_at from render_job where id = $1`, a.ID).Scan(&before)
	for round := 0; round < 5; round++ {
		if _, err := e.pool.Exec(e.ctx, `update render_job set status = 'queued', started_at = null where id in ($1,$2)`,
			a.ID, b.ID); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var got, full atomic.Int32
		for _, id := range []int64{a.ID, b.ID} {
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				_, err := r.ClaimRender(e.ctx, id, "v", time.Now().Add(2*time.Hour), 1)
				switch {
				case err == nil:
					got.Add(1)
				case errors.Is(err, timelinerepo.ErrRenderFull):
					full.Add(1)
				default:
					t.Error(err)
				}
			}(id)
		}
		wg.Wait()
		if got.Load() != 1 || full.Load() != 1 {
			t.Fatalf("第 %d 轮：领到 %d、满了 %d（上限 1）", round, got.Load(), full.Load())
		}
	}
	var after time.Time
	_ = e.pool.QueryRow(e.ctx, `select deadline_at from render_job where id = $1`, a.ID).Scan(&after)
	if !after.Equal(before) {
		t.Fatalf("领取不改死线：%v → %v", before, after)
	}
}

// ---------------------------------------------------------------- Worker（真 ffmpeg）

type localFetcher struct{ files map[int64]string }

func (f localFetcher) Fetch(_ context.Context, id int64) (domain.MediaFile, func(), error) {
	p, ok := f.files[id]
	if !ok {
		return domain.MediaFile{}, nil, errors.New("没有这份素材")
	}
	return domain.MediaFile{Path: p, HasVideo: true}, func() {}, nil
}

type countingArchiver struct {
	mu        sync.Mutex
	archived  int
	discarded []int64
	next      func() int64
	// keep 非空时把最后一份成片存到这里（量产物用）。
	keep string
}

func (a *countingArchiver) Archive(_ context.Context, _, _ int64, _ string, _ int64, body io.Reader) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.keep != "" {
		f, err := os.Create(a.keep)
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(f, body)
		_ = f.Close()
	} else {
		_, _ = io.Copy(io.Discard, body)
	}
	a.archived++
	return a.next(), nil
}

func (a *countingArchiver) Discard(_ context.Context, _, id int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.discarded = append(a.discarded, id)
	return nil
}

func needFF(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("本机没有 ffmpeg")
	}
	return t.TempDir()
}

func makeVideo(t *testing.T, dir, name, src string, secs int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", src,
		"-t", itoa(secs), "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", p).CombinedOutput(); err != nil {
		t.Fatalf("造素材：%v %s", err, out)
	}
	return p
}

func itoa(n int) string { return strconv.Itoa(n) }

func (e env) renderer(files map[int64]string) (*timelineapp.Renderer, *countingArchiver) {
	r := timelineapp.NewRenderer(timelinerepo.New(e.pool, e.next), slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.SetFetcher(localFetcher{files})
	arch := &countingArchiver{next: e.next}
	r.SetArchiver(arch)
	r.SetNextID(e.next)
	return r, arch
}

// SC-AC-23：渲染中取消，ffmpeg 在数秒内退出、不产生成片、状态为已取消（SCB-5）。
func TestRender_取消即停(t *testing.T) {
	dir := needFF(t)
	e := renderSetup(t, timelineapp.RenderLimits{})
	long := makeVideo(t, dir, "long.mp4", "testsrc2=size=1280x720:rate=30", 60)
	e.svc.SetMedia(fakeMedia{301: {Kind: "video", DurationMs: 60000}})
	v, err := e.svc.Create(e.ctx, timelineapp.CreateInput{WorkspaceID: ws, ProjectID: 777, AspectRatio: "16:9"})
	if err != nil {
		t.Fatal(err)
	}
	v = e.add(t, v, domain.TrackVideo, 301)
	job, err := e.start(v, "")
	if err != nil {
		t.Fatal(err)
	}
	r, arch := e.renderer(map[int64]string{301: long})
	done := make(chan time.Time, 1)
	go func() {
		_, _ = r.Run(e.ctx, job.ID)
		done <- time.Now()
	}()
	// 等到编码阶段再取消。
	deadline := time.Now().Add(30 * time.Second)
	for {
		var stage string
		_ = e.pool.QueryRow(e.ctx, `select coalesce(stage,'') from render_job where id = $1`, job.ID).Scan(&stage)
		if stage == domain.StageEncoding {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("没等到编码阶段：%q", stage)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancelledAt := time.Now()
	if _, err := e.svc.CancelRender(e.ctx, ws, job.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case at := <-done:
		if at.Sub(cancelledAt) > 5*time.Second {
			t.Fatalf("取消后 %s 才停", at.Sub(cancelledAt))
		}
		t.Logf("取消后 %s 停下", at.Sub(cancelledAt).Round(time.Millisecond))
	case <-time.After(20 * time.Second):
		t.Fatal("取消后渲染没有停")
	}
	var st string
	_ = e.pool.QueryRow(e.ctx, `select status from render_job where id = $1`, job.ID).Scan(&st)
	if st != domain.RenderCancelled || arch.archived != 0 {
		t.Fatalf("已取消、不归档：%s %d", st, arch.archived)
	}
}

// SCK-9：绑定了画布节点的时间线导出后不登记作品；没绑的照旧登记。导出后自检写进任务（只警告）。
func TestRender_绑定节点不登记作品_自检(t *testing.T) {
	dir := needFF(t)
	e := renderSetup(t, timelineapp.RenderLimits{})
	clip := makeVideo(t, dir, "c.mp4", "testsrc2=size=640x360:rate=25", 2)
	e.svc.SetMedia(fakeMedia{401: {Kind: "video", DurationMs: 2000}})
	mk := func(node string) timelineapp.View {
		v, err := e.svc.Create(e.ctx, timelineapp.CreateInput{WorkspaceID: ws, ProjectID: 777, AspectRatio: "16:9"})
		if err != nil {
			t.Fatal(err)
		}
		if node != "" {
			e.exec(t, `update timeline set canvas_node_id = $2 where id = $1`, v.Timeline.ID, node)
		}
		return e.add(t, v, domain.TrackVideo, 401)
	}
	bound, plain := mk("sc_node"), mk("")
	r, arch := e.renderer(map[int64]string{401: clip})
	for _, v := range []timelineapp.View{bound, plain} {
		job, err := e.start(v, "")
		if err != nil {
			t.Fatal(err)
		}
		if out, err := r.Run(e.ctx, job.ID); err != nil || out != timelineapp.RenderDone {
			t.Fatalf("渲染：%v %v", out, err)
		}
		got, _ := e.svc.GetRender(e.ctx, ws, job.ID)
		var check domain.SelfCheck
		if got.Status != domain.RenderSuccess || json.Unmarshal(got.SelfCheck, &check) != nil || check.DurationMs < 1900 {
			t.Fatalf("成功并带自检：%s %s", got.Status, got.SelfCheck)
		}
		if _, err := e.pool.Exec(e.ctx, `update render_job set status = 'success' where id = $1`, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	var works int
	_ = e.pool.QueryRow(e.ctx, `select count(*) from work where source_type = 'render'`).Scan(&works)
	if works != 1 || arch.archived != 2 {
		t.Fatalf("只有没绑节点的那条登记作品：%d 条、归档 %d 份", works, arch.archived)
	}
}

func (e env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		t.Fatalf("%s：%v", sql, err)
	}
}

type eventRecorder struct {
	mu     sync.Mutex
	events []timelineapp.RenderUpdate
}

func (r *eventRecorder) Publish(_ context.Context, topic string, payload []byte) error {
	var ev timelineapp.RenderUpdate
	if err := json.Unmarshal(payload, &ev); err != nil {
		return err
	}
	if topic != "project:777" {
		return errors.New("投错了主题：" + topic)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

// SC-AC-21：9:16、720p、25fps 导出的成片 ffprobe 为 720×1280、25fps。SC-AC-24：进度事件单调不减、最后是成功。
// SC-AC-29：带 2 秒黑场的成片照常成功，自检带「黑场」提醒，不判失败。
func TestRender_规格事件与黑场自检(t *testing.T) {
	dir := needFF(t)
	e := renderSetup(t, timelineapp.RenderLimits{})
	black := makeVideo(t, dir, "black.mp4", "color=c=black:s=640x360:rate=25", 2)
	color := makeVideo(t, dir, "color.mp4", "testsrc2=size=640x360:rate=25", 2)
	e.svc.SetMedia(fakeMedia{501: {Kind: "video", DurationMs: 2000}, 502: {Kind: "video", DurationMs: 2000}})
	rec := &eventRecorder{}
	v, err := e.svc.Create(e.ctx, timelineapp.CreateInput{WorkspaceID: ws, ProjectID: 777, AspectRatio: "9:16"})
	if err != nil {
		t.Fatal(err)
	}
	v = e.add(t, v, domain.TrackVideo, 501)
	v = e.add(t, v, domain.TrackVideo, 502)
	job, _, err := e.svc.StartRender(e.ctx, timelineapp.RenderInput{WorkspaceID: ws, PayerAccountID: 1, UserID: renderUser,
		TimelineID: v.Timeline.ID, Resolution: "720p", FPS: 25})
	if err != nil {
		t.Fatal(err)
	}
	r, arch := e.renderer(map[int64]string{501: black, 502: color})
	arch.keep = filepath.Join(dir, "out.mp4")
	r.SetEvents(rec)
	if out, err := r.Run(e.ctx, job.ID); err != nil || out != timelineapp.RenderDone {
		t.Fatalf("渲染：%v %v", out, err)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries",
		"stream=width,height,r_frame_rate", "-of", "csv=p=0", arch.keep).Output()
	if err != nil || string(probe) != "720,1280,25/1\n" {
		t.Fatalf("成片规格 720×1280、25fps：%q %v", probe, err)
	}
	got, _ := e.svc.GetRender(e.ctx, ws, job.ID)
	var check domain.SelfCheck
	_ = json.Unmarshal(got.SelfCheck, &check)
	if got.Status != domain.RenderSuccess || len(check.BlackSpans) != 1 || check.BlackSpans[0].EndMs < 1900 ||
		len(check.Warnings) == 0 {
		t.Fatalf("黑场只警告、照常成功：%s %s", got.Status, got.SelfCheck)
	}
	last := -1
	for _, ev := range rec.events {
		if ev.Progress < last {
			t.Fatalf("进度回退：%+v", rec.events)
		}
		last = ev.Progress
	}
	if n := len(rec.events); n < 3 || rec.events[n-1].Status != domain.RenderSuccess || rec.events[n-1].Output == nil {
		t.Fatalf("最后一条是成功并带成片：%+v", rec.events)
	}
}

// SC-AC-27（后端）：下载链接是 5 分钟的签名直链、带附件文件名；别的工作区 404；没成功的 409。
func TestRender_下载链接(t *testing.T) {
	e := renderSetup(t, timelineapp.RenderLimits{})
	store, _ := testsupport.FakeObjectStore(t)
	media := mediaapp.New(mediarepo.New(e.pool), store, e.next)
	e.svc.SetRenderDownloads(media, 5*time.Minute)
	v := e.timelineWithClip(t)
	job, err := e.start(v, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.svc.RenderDownload(e.ctx, ws, job.ID); codeOf(err) != errcode.Conflict {
		t.Fatalf("还没成功回 409：%v", err)
	}
	out := e.next()
	e.exec(t, `insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes, purpose,
		created_by_user_id) values ($1,$2,'seeaitv','render/x.mp4','video/mp4',10,'render_output',1)`, out, ws)
	e.exec(t, `update render_job set status = 'success', output_media_id = $2 where id = $1`, job.ID, out)
	url, exp, name, err := e.svc.RenderDownload(e.ctx, ws, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "X-Amz-Expires=300") || !strings.Contains(url, "response-content-disposition=attachment") ||
		!strings.HasSuffix(name, ".mp4") || time.Until(exp) > 5*time.Minute || time.Until(exp) < 4*time.Minute {
		t.Fatalf("5 分钟、附件下载：%s %s %v", url, name, exp)
	}
	if _, _, _, err := e.svc.RenderDownload(e.ctx, ws+1, job.ID); codeOf(err) != errcode.NotFound {
		t.Fatalf("别的工作区 404：%v", err)
	}
}

// SCT-12 端到端：画面无声，配乐放在 2.5 秒、人声在 4 秒。成片带音轨且与画面等长；0~2.5 秒没有声音、
// 2.5 秒起有配乐（AC-12）；整片响度在 -14 ± 1 LUFS（AC-16 的响度部分；闪避深度由 domain 的量产物测试量）。
func TestRender_音频预渲端到端(t *testing.T) {
	dir := needFF(t)
	e := renderSetup(t, timelineapp.RenderLimits{})
	video := makeVideo(t, dir, "v.mp4", "testsrc2=size=640x360:rate=25", 6)
	gen := func(name, src string) string {
		p := filepath.Join(dir, name)
		if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", src,
			"-c:a", "pcm_s16le", p).CombinedOutput(); err != nil {
			t.Fatalf("造音频：%v %s", err, out)
		}
		return p
	}
	music := gen("music.wav", "sine=frequency=220:sample_rate=48000:duration=8")
	voice := gen("voice.wav", "sine=frequency=1000:sample_rate=48000:duration=2")
	e.svc.SetMedia(fakeMedia{601: {Kind: "video", DurationMs: 6000}, 602: {Kind: "audio", DurationMs: 8000},
		603: {Kind: "audio", DurationMs: 2000}})
	v, err := e.svc.Create(e.ctx, timelineapp.CreateInput{WorkspaceID: ws, ProjectID: 777, AspectRatio: "16:9"})
	if err != nil {
		t.Fatal(err)
	}
	v = e.add(t, v, domain.TrackVideo, 601)
	c := timelineapp.ContentFromView(v)
	for i := range c.Tracks {
		if c.Tracks[i].Kind == domain.TrackAudio {
			c.Tracks[i].Role = domain.RoleMusic
			c.Tracks[i].Clips = []domain.ContentClip{{ID: "tmp-m", MediaID: 602, SourceOutMs: 6000, StartMs: startAt(2500)}}
		}
	}
	c.Tracks = append(c.Tracks, domain.ContentTrack{ID: "tmp-voice", Kind: domain.TrackAudio, Role: domain.RoleVoice,
		Clips: []domain.ContentClip{{ID: "tmp-v", MediaID: 603, SourceOutMs: 2000, StartMs: startAt(4000)}}})
	if _, err := e.svc.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		Content: c}); err != nil {
		t.Fatal(err)
	}
	job, err := e.start(v, "")
	if err != nil {
		t.Fatal(err)
	}
	r, arch := e.renderer(nil)
	r.SetFetcher(fileFetcher{map[int64]domain.MediaFile{
		601: {Path: video, HasVideo: true},
		602: {Path: music, HasAudio: true, AudioChannels: 1},
		603: {Path: voice, HasAudio: true, AudioChannels: 1},
	}})
	arch.keep = filepath.Join(dir, "out.mp4")
	if out, err := r.Run(e.ctx, job.ID); err != nil || out != timelineapp.RenderDone {
		got, _ := e.svc.GetRender(e.ctx, ws, job.ID)
		t.Fatalf("渲染：%v %v %s", out, err, got.ErrorMessage)
	}
	probe, _ := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type,duration", "-of", "csv=p=0",
		arch.keep).Output()
	if !strings.Contains(string(probe), "audio") {
		t.Fatalf("成片要有音轨：%s", probe)
	}
	rms, _ := exec.Command("ffmpeg", "-hide_banner", "-i", arch.keep, "-vn", "-af",
		"asetnsamples=24000,astats=metadata=1:reset=1,ametadata=print:key=lavfi.astats.Overall.RMS_level:file=-",
		"-f", "null", "-").CombinedOutput()
	var levels []string
	for _, line := range strings.Split(string(rms), "\n") {
		if i := strings.Index(line, "RMS_level="); i >= 0 {
			levels = append(levels, line[i+len("RMS_level="):])
		}
	}
	// AAC 编码在静音段里会留一点底噪（-100dB 量级），按 -60dB 判「没有声音」。
	level := func(s string) float64 {
		if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return v
		}
		return -1000 // -inf
	}
	if len(levels) < 11 || level(levels[0]) > -60 || level(levels[4]) > -60 || level(levels[5]) < -30 {
		t.Fatalf("0~2.5 秒没有声音、2.5 秒起有配乐（0.5 秒窗）：%v", levels)
	}
	loud, _ := exec.Command("ffmpeg", "-hide_banner", "-nostats", "-i", arch.keep, "-vn", "-af", "ebur128=framelog=quiet",
		"-f", "null", "-").CombinedOutput()
	_, lufs := domain.ParseSelfCheck(loud)
	if lufs == nil || *lufs < -15 || *lufs > -13 {
		t.Fatalf("整片响度 -14±1 LUFS：%v", lufs)
	}
	t.Logf("整片响度 %.1f LUFS；0.5 秒窗：%v", *lufs, levels[:12])
}

type fileFetcher struct{ files map[int64]domain.MediaFile }

func (f fileFetcher) Fetch(_ context.Context, id int64) (domain.MediaFile, func(), error) {
	m, ok := f.files[id]
	if !ok {
		return domain.MediaFile{}, nil, errors.New("没有这份素材")
	}
	return m, func() {}, nil
}

// recordingTypesetter 记下渲染时排的每一条（核对与预览接口同一个缓存键）。
type recordingTypesetter struct {
	inner *typeset.Typesetter
	mu    sync.Mutex
	keys  map[string]bool
}

func (r *recordingTypesetter) Typeset(ctx context.Context, req typeset.Request) (*typeset.Result, error) {
	res, err := r.inner.Typeset(ctx, req)
	if err == nil {
		r.mu.Lock()
		r.keys[res.Key] = true
		r.mu.Unlock()
	}
	return res, err
}

// SC-AC-17：中文字幕与英文标题进成片（对应时段、对应位置有字）；两条字幕轨同时有字时上下叠放、不重叠；
// 渲染用的条目与排版预览接口是同一个缓存键（同一张图）。本机 ffmpeg 没有 libass 也照样导出。
func TestRender_字幕与文字叠加(t *testing.T) {
	dir := needFF(t)
	ts := typeset.New(typeset.Options{})
	if _, err := ts.Fonts(); err != nil {
		t.Skipf("排版字体没装（scripts/fetch-fonts.sh）：%v", err)
	}
	e := renderSetup(t, timelineapp.RenderLimits{})
	gray := makeVideo(t, dir, "gray.mp4", "color=c=gray:s=640x360:rate=25", 4)
	e.svc.SetMedia(fakeMedia{701: {Kind: "video", DurationMs: 4000}})
	v, err := e.svc.Create(e.ctx, timelineapp.CreateInput{WorkspaceID: ws, ProjectID: 777, AspectRatio: "16:9"})
	if err != nil {
		t.Fatal(err)
	}
	v = e.add(t, v, domain.TrackVideo, 701)
	c := timelineapp.ContentFromView(v)
	subTrack := ""
	for _, tr := range c.Tracks {
		if tr.Kind == domain.TrackSubtitle {
			subTrack = tr.ID
		}
	}
	c.Tracks = append(c.Tracks,
		domain.ContentTrack{ID: "tmp-sub2", Kind: domain.TrackSubtitle, Lang: "en"},
		domain.ContentTrack{ID: "tmp-text", Kind: domain.TrackText})
	red, blue := json.RawMessage(`{"color":"#FF0000"}`), json.RawMessage(`{"color":"#0000FF"}`)
	c.Subtitles = []domain.ContentSubtitle{
		{ID: "tmp-s1", TrackID: subTrack, StartMs: 0, EndMs: 3000, Text: "你好世界", Style: red},
		{ID: "tmp-s2", TrackID: "tmp-sub2", StartMs: 1000, EndMs: 3000, Text: "Hello world", Style: blue},
	}
	c.Texts = []domain.ContentText{{ID: "tmp-t", TrackID: "tmp-text", StartMs: 1000, EndMs: 3000, Text: "Title",
		Box: json.RawMessage(`{"xPct":50,"yPct":15,"anchor":"top"}`)}}
	if _, err := e.svc.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: v.Timeline.ID,
		Content: c}); err != nil {
		t.Fatal(err)
	}
	job, err := e.start(v, "")
	if err != nil {
		t.Fatal(err)
	}
	r, arch := e.renderer(map[int64]string{701: gray})
	rec := &recordingTypesetter{inner: ts, keys: map[string]bool{}}
	r.SetTypesetter(rec)
	arch.keep = filepath.Join(dir, "out.mp4")
	if out, err := r.Run(e.ctx, job.ID); err != nil || out != timelineapp.RenderDone {
		got, _ := e.svc.GetRender(e.ctx, ws, job.ID)
		t.Fatalf("渲染：%v %v %s", out, err, got.ErrorMessage)
	}

	// 与预览接口同一个缓存键：前端按「轨道样式 ← 条目样式」合并后发给 /smartcut/typeset 的就是这一条。
	preview, err := ts.Typeset(e.ctx, typeset.Request{Text: "你好世界", Kind: typeset.KindSubtitle,
		Style: map[string]any{"color": "#FF0000"}, Width: 1920, Height: 1080})
	if err != nil || !rec.keys[preview.Key] {
		t.Fatalf("渲染用的条目应与预览同一个缓存键：%v %v", err, rec.keys)
	}

	// 2 秒那一帧：画面正中一列上，红字（字幕 1）在下、蓝字（字幕 2）在上，两段不重叠；3.5 秒时都没了。
	column := func(at float64) []byte {
		out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-ss", strconv.FormatFloat(at, 'f', 2, 64),
			"-i", arch.keep, "-frames:v", "1", "-vf", "crop=200:1080:860:0", "-f", "rawvideo", "-pix_fmt", "rgb24", "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	rows := func(px []byte, want func(r, g, b int) bool) (lo, hi int) {
		lo, hi = -1, -1
		for y := 0; y < 1080; y++ {
			for x := 0; x < 200; x++ {
				i := (y*200 + x) * 3
				if want(int(px[i]), int(px[i+1]), int(px[i+2])) {
					if lo < 0 {
						lo = y
					}
					hi = y
					break
				}
			}
		}
		return lo, hi
	}
	isRed := func(r, g, b int) bool { return r > 180 && g < 90 && b < 90 }
	isBlue := func(r, g, b int) bool { return b > 180 && r < 90 && g < 90 }
	f2 := column(2.0)
	rlo, rhi := rows(f2, isRed)
	blo, bhi := rows(f2, isBlue)
	if rlo < 0 || blo < 0 || bhi >= rlo || rhi < 1080/2 {
		t.Fatalf("两条字幕都在、蓝在上红在下且不重叠：红 %d~%d、蓝 %d~%d", rlo, rhi, blo, bhi)
	}
	f35 := column(3.5)
	if lo, _ := rows(f35, isRed); lo >= 0 {
		t.Fatalf("3.5 秒字幕已结束，不该还有红字（第 %d 行）", lo)
	}
	t.Logf("2 秒：红字 %d~%d 行、蓝字 %d~%d 行", rlo, rhi, blo, bhi)
}
