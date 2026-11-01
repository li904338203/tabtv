//go:build integration

// 3D 导演台的复制与清理（docs/agent/director，DRT-04，DRD-30）的真库测试。
//
// ⚠️ 必须真库：复制与清理都是「一个事务」的承诺，回滚、for update、按项目 ID 批量删，都由 SQL 本身决定。
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	director "github.com/li904338203/tabtv/backend/internal/director/domain"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
)

// mediaBackedStore 上传时在 media_object 里写一行（不碰对象存储），删除走真的媒体域 Delete（含引用检查器）。
type mediaBackedStore struct {
	mr    *mediarepo.Repo
	media *mediaapp.Service
	next  func() int64
}

func (s mediaBackedStore) Upload(ctx context.Context, in canvasapp.DirectorUploadInput) (canvasapp.UploadedMedia, error) {
	id := s.next()
	err := s.mr.Insert(ctx, mediarepo.MediaObject{ID: id, WorkspaceID: in.WorkspaceID, Bucket: "b",
		ObjectKey: in.Purpose + "/" + itoa10(id), ContentType: "image/png", SizeBytes: 1, Purpose: in.Purpose,
		CreatedByUserID: in.UserID})
	return canvasapp.UploadedMedia{ID: id, MIME: "image/png", Width: 64, Height: 36}, err
}

func (s mediaBackedStore) Delete(ctx context.Context, ws, id int64) error {
	return s.media.Delete(ctx, ws, id)
}

// copySetup 是带真实媒体删除（与导演台检查器）的画布服务。
func copySetup(t *testing.T) (*canvasapp.Service, *repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	r, pool, next := setup(t)
	media := mediaapp.New(mediarepo.New(pool), nil, next)
	media.AddReferenceChecker(refChecker{mediaapp.UsageDirectorScene, r.DirectorMediaReferences})
	svc := canvasapp.New(r, next)
	svc.SetDirectorMedia(mediaBackedStore{mr: mediarepo.New(pool), media: media, next: next})
	return svc, r, pool, next
}

// projectWithDirector 建一个项目：画布上一个导演台节点（指向它的场景）与一个恰好也带 sceneId 的文本节点，
// 场景里有一张截图。返回项目 ID、场景与截图记录。
func projectWithDirector(t *testing.T, svc *canvasapp.Service, r *repo.Repo, next func() int64) (
	int64, canvasapp.DirectorSceneView, repo.DirectorMedia) {
	t.Helper()
	ctx := context.Background()
	pid := mustInsert(t, r, next, ws, "有导演台的项目")
	sc := newScene(t, svc, ws, pid, "n_dir")
	rec, err := svc.UploadDirectorCapture(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, canvasapp.DirectorCaptureInput{Kind: "capture", CameraID: defaultCameraID(t),
			SceneVersion: 1, File: file("png")})
	if err != nil {
		t.Fatal(err)
	}
	sid := itoa10(sc.Scene.ID)
	canvas := json.RawMessage(`{"nodes":[
		{"id":"n_dir","type":"director","params":{"sceneId":"` + sid + `","outputMediaId":"` + itoa10(rec.MediaID) + `"}},
		{"id":"n_txt","type":"text","params":{"sceneId":"` + sid + `"}}],"edges":[]}`)
	p, err := r.Get(ctx, ws, pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveCanvas(ctx, ws, pid, p.Version, 1, canvas); err != nil {
		t.Fatal(err)
	}
	return pid, sc, rec
}

func nodeParams(t *testing.T, canvas json.RawMessage, nodeID string) map[string]any {
	t.Helper()
	var doc struct {
		Nodes []struct {
			ID     string         `json:"id"`
			Params map[string]any `json:"params"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(canvas, &doc); err != nil {
		t.Fatal(err)
	}
	for _, n := range doc.Nodes {
		if n.ID == nodeID {
			return n.Params
		}
	}
	t.Fatalf("画布里没有节点 %s", nodeID)
	return nil
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mediaStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `select status from media_object where id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func duplicate(t *testing.T, svc *canvasapp.Service, r *repo.Repo, pid int64) (repo.Project, json.RawMessage) {
	t.Helper()
	ctx := context.Background()
	cp, err := svc.Duplicate(ctx, ws, 1, pid, collab.RoleOwner, "")
	if err != nil {
		t.Fatal(err)
	}
	full, err := r.GetWithCanvas(ctx, ws, cp.ID)
	if err != nil {
		t.Fatal(err)
	}
	return cp, full.CanvasData
}

func TestDuplicate_复制导演台场景与记录(t *testing.T) {
	svc, r, _, next := copySetup(t)
	ctx := context.Background()
	pid, src, rec := projectWithDirector(t, svc, r, next)
	// 源场景先存过几版：副本要从 1 起。
	if _, err := svc.UpdateDirectorScene(ctx, ws, pid, src.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 1, director.DefaultScene()); err != nil {
		t.Fatal(err)
	}

	cp, canvas := duplicate(t, svc, r, pid)
	newID := nodeParams(t, canvas, "n_dir")["sceneId"].(string)
	if newID == itoa10(src.Scene.ID) {
		t.Fatal("副本导演台节点的 sceneId 应指向新场景")
	}
	list, err := svc.ListDirectorScenes(ctx, ws, cp.ID, collab.RoleViewer)
	if err != nil || len(list) != 1 || itoa10(list[0].ID) != newID {
		t.Fatalf("副本项目里应恰好一个场景且就是节点指向的那个：%+v %v", list, err)
	}
	v, err := svc.GetDirectorScene(ctx, ws, cp.ID, list[0].ID, collab.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if v.Scene.ProjectID != cp.ID || v.Scene.Version != 1 || v.Scene.Source != "user" || v.Scene.EditorID != 0 ||
		v.Scene.CanvasNodeID != "n_dir" || v.Scene.RenderFingerprint != list[0].RenderFingerprint {
		t.Fatalf("副本场景：属于副本项目、版本 1、来源 user、不带编辑者、节点 ID 不变，得到 %+v", v.Scene)
	}
	if len(v.Records) != 1 || v.Records[0].MediaID != rec.MediaID || v.Records[0].ID == rec.ID {
		t.Fatalf("记录条数与媒体 ID 应与源相同、记录 ID 不同，得到 %+v", v.Records)
	}
}

func TestDuplicate_副本场景独立(t *testing.T) {
	svc, r, _, next := copySetup(t)
	ctx := context.Background()
	pid, src, _ := projectWithDirector(t, svc, r, next)
	cp, canvas := duplicate(t, svc, r, pid)
	newID := nodeParams(t, canvas, "n_dir")["sceneId"].(string)
	var copyID int64
	for _, s := range mustList(t, svc, cp.ID) {
		if itoa10(s.ID) == newID {
			copyID = s.ID
		}
	}
	changed := editedScene(t, func(m map[string]any) { m["stage"] = map[string]any{"scale": 3} })
	if _, err := svc.UpdateDirectorScene(ctx, ws, cp.ID, copyID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 1, changed); err != nil {
		t.Fatal(err)
	}
	orig, _ := svc.GetDirectorScene(ctx, ws, pid, src.Scene.ID, collab.RoleViewer)
	if !sameJSON(t, orig.Scene.Scene, director.DefaultScene()) || orig.Scene.Version != 1 {
		t.Fatalf("改副本不该改到源场景，源现在是 version=%d %s", orig.Scene.Version, orig.Scene.Scene)
	}
}

func mustList(t *testing.T, svc *canvasapp.Service, pid int64) []repo.DirectorSceneSummary {
	t.Helper()
	list, err := svc.ListDirectorScenes(context.Background(), ws, pid, collab.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestDuplicate_重绑只改导演台节点(t *testing.T) {
	svc, r, _, next := copySetup(t)
	pid, src, rec := projectWithDirector(t, svc, r, next)
	_, canvas := duplicate(t, svc, r, pid)
	if got := nodeParams(t, canvas, "n_txt")["sceneId"]; got != itoa10(src.Scene.ID) {
		t.Fatalf("文本节点里恰好叫 sceneId 的键不能被改（DRR-15），得到 %v", got)
	}
	// 截图的镜像原样保留：副本有自己的记录保护着同一份文件。
	if got := nodeParams(t, canvas, "n_dir")["outputMediaId"]; got != itoa10(rec.MediaID) {
		t.Fatalf("导演台节点的 outputMediaId 应原样保留，得到 %v", got)
	}
}

func TestPurge_一并删除导演台三表(t *testing.T) {
	svc, r, pool, next := copySetup(t)
	ctx := context.Background()
	pid, src, _ := projectWithDirector(t, svc, r, next)
	if _, err := pool.Exec(ctx, `
		insert into director_llm_call (id, workspace_id, project_id, scene_id, user_id, source, model_code, prompt_chars)
		values ($1, $2, $3, $4, 1, 'web', 'm', 10)`, next(), ws, pid, src.Scene.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.SoftDelete(ctx, ws, pid); err != nil {
		t.Fatal(err)
	}
	if err := svc.Purge(ctx, ws, 1, pid, collab.RoleOwner); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"director_scene", "director_media", "director_llm_call"} {
		if n := count(t, pool, `select count(*) from `+table+` where project_id = $1`, pid); n != 0 {
			t.Errorf("彻底删除之后 %s 应为 0 行，得到 %d", table, n)
		}
	}
	if _, err := r.Get(ctx, ws, pid); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("项目应已删除，得到 %v", err)
	}
}

func TestPurge_失败整体回滚(t *testing.T) {
	svc, r, pool, next := copySetup(t)
	ctx := context.Background()
	pid, _, _ := projectWithDirector(t, svc, r, next)
	if err := r.SoftDelete(ctx, ws, pid); err != nil {
		t.Fatal(err)
	}
	// 让「删场景」这一步在事务中途失败：前面已经删掉的媒体记录必须跟着回滚。
	if _, err := pool.Exec(ctx, `
		create or replace function test_fail_director_delete() returns trigger language plpgsql as $$
		begin raise exception 'test: 模拟清理中途失败'; end $$;
		create trigger test_fail_director_delete before delete on director_scene
		for each row execute function test_fail_director_delete();`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `drop trigger if exists test_fail_director_delete on director_scene;
			drop function if exists test_fail_director_delete();`)
	})
	if err := svc.Purge(ctx, ws, 1, pid, collab.RoleOwner); err == nil {
		t.Fatal("中途失败的清理应报错")
	}
	p, err := r.Get(ctx, ws, pid)
	if err != nil || p.Status != "deleted" {
		t.Fatalf("项目应仍在回收站里，得到 %+v %v", p, err)
	}
	if n := count(t, pool, `select count(*) from director_media where project_id = $1`, pid); n != 1 {
		t.Fatalf("导演台记录一条都不该少，得到 %d", n)
	}
	if n := count(t, pool, `select count(*) from director_scene where project_id = $1`, pid); n != 1 {
		t.Fatalf("场景一个都不该少，得到 %d", n)
	}
}

func TestPurgeExpired_批量删除导演台行(t *testing.T) {
	svc, r, pool, next := copySetup(t)
	ctx := context.Background()
	var pids []int64
	var media []int64
	for i := 0; i < 2; i++ {
		pid, _, rec := projectWithDirector(t, svc, r, next)
		pids = append(pids, pid)
		media = append(media, rec.MediaID)
		if err := r.SoftDelete(ctx, ws, pid); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `update project set deleted_at = now() - interval '40 days' where id = any($1)`,
		pids); err != nil {
		t.Fatal(err)
	}
	c := canvasapp.NewCleaner(svc, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), nil)
	if n := c.Once(ctx); n != 2 {
		t.Fatalf("应清掉两个过期项目，得到 %d", n)
	}
	for _, table := range []string{"director_scene", "director_media"} {
		if n := count(t, pool, `select count(*) from `+table+` where project_id = any($1)`, pids); n != 0 {
			t.Errorf("按保留期清理之后 %s 应为 0 行，得到 %d", table, n)
		}
	}
	for _, id := range media {
		if s := mediaStatus(t, pool, id); s != "deleted" {
			t.Errorf("没有别处引用的导演台截图应进入软删，得到 %s", s)
		}
	}
}

func TestPurge_清理后导演台媒体进入软删(t *testing.T) {
	svc, r, pool, next := copySetup(t)
	ctx := context.Background()
	pid, _, rec := projectWithDirector(t, svc, r, next)
	if s := mediaStatus(t, pool, rec.MediaID); s != "active" {
		t.Fatalf("上传后应为 active，得到 %s", s)
	}
	if err := r.SoftDelete(ctx, ws, pid); err != nil {
		t.Fatal(err)
	}
	if err := svc.Purge(ctx, ws, 1, pid, collab.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if s := mediaStatus(t, pool, rec.MediaID); s != "deleted" {
		t.Fatalf("彻底删除之后，没有别处引用的截图应进入软删，得到 %s", s)
	}
}

func TestPurge_被复制项目引用的媒体保留(t *testing.T) {
	svc, r, pool, next := copySetup(t)
	ctx := context.Background()
	pid, _, rec := projectWithDirector(t, svc, r, next)
	duplicate(t, svc, r, pid)
	if err := r.SoftDelete(ctx, ws, pid); err != nil {
		t.Fatal(err)
	}
	if err := svc.Purge(ctx, ws, 1, pid, collab.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if s := mediaStatus(t, pool, rec.MediaID); s != "active" {
		t.Fatalf("副本项目还引用着这张截图，它应保持 active，得到 %s", s)
	}
}

// failingStore 的删除总是失败（媒体域不可用）。
type failingStore struct{ fakeDirectorStore }

func (f *failingStore) Delete(context.Context, int64, int64) error {
	return errors.New("媒体域暂时不可用")
}

// syncBuffer 是可并发写的日志缓冲。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCleanerOnce_释放失败只告警(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	svc := canvasapp.New(r, next)
	svc.SetLogger(logger)
	store := &failingStore{fakeDirectorStore{next: next}}
	svc.SetDirectorMedia(store)
	pid, _, _ := projectWithDirector(t, svc, r, next)
	if err := r.SoftDelete(ctx, ws, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update project set deleted_at = now() - interval '40 days' where id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	if n := canvasapp.NewCleaner(svc, logger, nil).Once(ctx); n != 1 {
		t.Fatalf("释放失败不影响清理本身，应清掉 1 个项目，得到 %d", n)
	}
	if n := count(t, pool, `select count(*) from director_scene where project_id = $1`, pid); n != 0 {
		t.Fatalf("清理已经提交，场景应为 0 行，得到 %d", n)
	}
	if !strings.Contains(logs.String(), "释放导演台文件失败") {
		t.Fatalf("释放失败要留告警日志，得到 %s", logs.String())
	}
}
