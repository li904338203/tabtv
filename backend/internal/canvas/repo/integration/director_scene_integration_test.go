//go:build integration

// 3D 导演台场景（docs/agent/director，DRT-02）的真库测试。
//
// ⚠️ 必须真库：乐观锁的 0 行判定、(项目, 节点) 唯一索引上的幂等建、workspace / project 双条件、
// `scene->>'outputCameraId'` —— 都由 SQL 本身决定。乐观锁要在**真并发**下测：顺序调用永远先撞上
// 状态检查，那种测试把版本条件整条删掉照样绿（CLAUDE.md §5.7）。
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	director "github.com/li904338203/tabtv/backend/internal/director/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// wsMedia 是只认本工作区文件的假媒体查询：ID → 所属工作区与类型。
type wsMedia map[int64]canvasapp.MediaInfo

func (m wsMedia) LookupInWorkspace(_ context.Context, ws, id int64) (canvasapp.MediaInfo, error) {
	info, ok := m[id]
	if !ok || (info.WorkspaceID != ws && info.WorkspaceID != 0) {
		return canvasapp.MediaInfo{}, errors.New("not found")
	}
	return info, nil
}

func newUser(t *testing.T, pool *pgxpool.Pool, next func() int64, nickname string) int64 {
	t.Helper()
	id := next()
	if _, err := pool.Exec(context.Background(), `
		insert into app_user (id, phone, nickname, password_hash) values ($1, $2, $3, 'x')`,
		id, "138"+itoa10(id%100000000), nickname); err != nil {
		t.Fatal(err)
	}
	return id
}

func newScene(t *testing.T, svc *canvasapp.Service, workspace, projectID int64, nodeID string) canvasapp.DirectorSceneView {
	t.Helper()
	v, created, err := svc.CreateDirectorScene(context.Background(), workspace, projectID,
		canvasapp.DirectorActor{UserID: 1}, collab.RoleOwner, canvasapp.CreateDirectorSceneInput{CanvasNodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatalf("节点 %s 应是新建", nodeID)
	}
	return v
}

// editedScene 在默认场景上改几处（输出机位改名、加一个未知字段）。
func editedScene(t *testing.T, edit func(m map[string]any)) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(director.DefaultScene(), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

func TestDirectorScene_真并发保存只有一个成功(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")

	const n = 8
	editors := make([]int64, n)
	for i := range editors {
		editors[i] = newUser(t, pool, next, "编辑"+itoa10(int64(i)))
	}
	bodies := make([]json.RawMessage, n)
	for i := range bodies {
		bodies[i] = editedScene(t, func(m map[string]any) { m["note"] = "第 " + itoa10(int64(i)) + " 份" })
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = svc.UpdateDirectorScene(ctx, ws, pid, sc.Scene.ID,
				canvasapp.DirectorActor{UserID: editors[i]}, collab.RoleEditor, 1, bodies[i])
		}(i)
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner >= 0 {
				t.Fatalf("同一版本有两个保存都成功了：%d 与 %d", winner, i)
			}
			winner = i
		}
	}
	if winner < 0 {
		t.Fatalf("应恰好一个成功，实际全部失败：%v", errs)
	}
	for i, err := range errs {
		if i == winner {
			continue
		}
		var be *httpx.BizError
		if !errors.As(err, &be) || be.Code != errcode.VersionConflict {
			t.Fatalf("第 %d 个应是 40902，得到 %v", i, err)
		}
		// DR-AC-11：失败方看得到「是谁改的、现在是第几版」。
		if be.Details["currentVersion"] != 2 || be.Details["currentEditorName"] != "编辑"+itoa10(int64(winner)) {
			t.Errorf("冲突详情应指向赢家（第 2 版、编辑%d），得到 %v", winner, be.Details)
		}
		if be.Details["bySelf"] != false || be.Details["editorKind"] != "user" || be.Details["currentSource"] != "user" {
			t.Errorf("同事之间的冲突：bySelf=false、editorKind=currentSource=user，得到 %v", be.Details)
		}
	}
	cur, err := svc.GetDirectorScene(ctx, ws, pid, sc.Scene.ID, collab.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Scene.Version != 2 || cur.Scene.EditorID != editors[winner] {
		t.Fatalf("版本只能进一位、内容是赢家那份，得到 version=%d editor=%d", cur.Scene.Version, cur.Scene.EditorID)
	}
}

func TestDirectorScene_同节点重复建返回同一份(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	pid := mustInsert(t, r, next, ws, "项目")

	// 并发建同一个节点（两个标签页同时打开一个粘贴出来的节点）：收敛到同一份，恰好一次是新建。
	const n = 6
	var wg sync.WaitGroup
	ids := make([]int64, n)
	created := make([]bool, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			v, c, err := svc.CreateDirectorScene(ctx, ws, pid, canvasapp.DirectorActor{UserID: 1}, collab.RoleEditor,
				canvasapp.CreateDirectorSceneInput{CanvasNodeID: "n_same"})
			ids[i], created[i], errs[i] = v.Scene.ID, c, err
		}(i)
	}
	close(start)
	wg.Wait()
	newCount := 0
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("第 %d 次建场景失败：%v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("同一节点应收敛到同一个场景，得到 %d 与 %d", ids[i], ids[0])
		}
		if created[i] {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("恰好一次是新建（201），其余是已有（200），实际新建 %d 次", newCount)
	}

	// 默认场景：版本 1、带指纹、来源 user。
	v, err := svc.GetDirectorScene(ctx, ws, pid, ids[0], collab.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if v.Scene.Version != 1 || v.Scene.RenderFingerprint == "" || v.Scene.Source != "user" ||
		v.Scene.CanvasNodeID != "n_same" || len(v.Records) != 0 {
		t.Fatalf("新场景状态不对：%+v", v.Scene)
	}
	if !sameJSON(t, v.Scene.Scene, director.DefaultScene()) {
		t.Fatalf("新场景应是默认场景，得到 %s", v.Scene.Scene)
	}

	// 节点 ID 非法。
	_, _, err = svc.CreateDirectorScene(ctx, ws, pid, canvasapp.DirectorActor{UserID: 1}, collab.RoleEditor,
		canvasapp.CreateDirectorSceneInput{CanvasNodeID: "  "})
	if codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("空节点 ID 应 40001，得到 %v", err)
	}
	// 只读成员不能建。
	_, _, err = svc.CreateDirectorScene(ctx, ws, pid, canvasapp.DirectorActor{UserID: 1}, collab.RoleViewer,
		canvasapp.CreateDirectorSceneInput{CanvasNodeID: "n_viewer"})
	if codeOf(err) != errcode.PermissionDenied {
		t.Fatalf("viewer 建场景应 403，得到 %v", err)
	}
}

func TestDirectorScene_分叉复制源场景(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	p1 := mustInsert(t, r, next, ws, "源项目")
	p2 := mustInsert(t, r, next, ws, "另一个项目")
	src := newScene(t, svc, ws, p1, "n_src")
	body := editedScene(t, func(m map[string]any) {
		m["note"] = map[string]any{"未知": "字段"}
		m["stage"] = map[string]any{"scale": 2}
	})
	saved, err := svc.UpdateDirectorScene(ctx, ws, p1, src.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 1, body)
	if err != nil {
		t.Fatal(err)
	}

	// 跨项目粘贴：源在 p1，新节点在 p2。
	fork, created, err := svc.CreateDirectorScene(ctx, ws, p2, canvasapp.DirectorActor{UserID: 1}, collab.RoleEditor,
		canvasapp.CreateDirectorSceneInput{CanvasNodeID: "n_copy", SourceSceneID: src.Scene.ID})
	if err != nil || !created {
		t.Fatalf("分叉应新建：created=%v err=%v", created, err)
	}
	if fork.Scene.ID == src.Scene.ID || fork.Scene.ProjectID != p2 || fork.Scene.Version != 1 {
		t.Fatalf("分叉应是 p2 里的新场景、版本从 1 起，得到 %+v", fork.Scene)
	}
	if !sameJSON(t, fork.Scene.Scene, body) {
		t.Fatalf("分叉的正文应与源相同（含未知字段），得到 %s", fork.Scene.Scene)
	}
	if fork.Scene.RenderFingerprint != saved.RenderFingerprint {
		t.Errorf("正文相同，指纹应相同：%s vs %s", fork.Scene.RenderFingerprint, saved.RenderFingerprint)
	}
	// 改副本不影响源。
	if _, err := svc.UpdateDirectorScene(ctx, ws, p2, fork.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 1, director.DefaultScene()); err != nil {
		t.Fatal(err)
	}
	again, _ := svc.GetDirectorScene(ctx, ws, p1, src.Scene.ID, collab.RoleViewer)
	if !sameJSON(t, again.Scene.Scene, body) {
		t.Fatal("改副本不该改到源场景")
	}

	// 源项目进了回收站：仍可读，所以仍能分叉。
	if err := r.SoftDelete(ctx, ws, p1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateDirectorScene(ctx, ws, p2, canvasapp.DirectorActor{UserID: 1}, collab.RoleEditor,
		canvasapp.CreateDirectorSceneInput{CanvasNodeID: "n_copy2", SourceSceneID: src.Scene.ID}); err != nil {
		t.Fatalf("回收站里的源场景也算可读，应能分叉：%v", err)
	}
}

func TestDirectorScene_跨工作区源场景不能分叉(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	other := mustInsert(t, r, next, ws+1, "别人的项目")
	theirs := newScene(t, svc, ws+1, other, "n_theirs")
	mine := mustInsert(t, r, next, ws, "我的项目")

	_, _, err := svc.CreateDirectorScene(ctx, ws, mine, canvasapp.DirectorActor{UserID: 1}, collab.RoleOwner,
		canvasapp.CreateDirectorSceneInput{CanvasNodeID: "n_steal", SourceSceneID: theirs.Scene.ID})
	if codeOf(err) != errcode.NotFound {
		t.Fatalf("别的工作区的源场景应 404，得到 %v", err)
	}
	_, _, err = svc.CreateDirectorScene(ctx, ws, mine, canvasapp.DirectorActor{UserID: 1}, collab.RoleOwner,
		canvasapp.CreateDirectorSceneInput{CanvasNodeID: "n_none", SourceSceneID: 424242})
	if codeOf(err) != errcode.NotFound {
		t.Fatalf("不存在的源场景应 404，得到 %v", err)
	}
}

func TestDirectorScene_回收站只读(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")
	if err := r.SoftDelete(ctx, ws, pid); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.GetDirectorScene(ctx, ws, pid, sc.Scene.ID, collab.RoleViewer); err != nil {
		t.Fatalf("回收站里的项目也能读场景（DR-F-171）：%v", err)
	}
	if list, err := svc.ListDirectorScenes(ctx, ws, pid, collab.RoleViewer); err != nil || len(list) != 1 {
		t.Fatalf("回收站里的项目也能列摘要：%v %v", list, err)
	}
	_, err := svc.UpdateDirectorScene(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 1, director.DefaultScene())
	if codeOf(err) != errcode.InvalidStateTransition {
		t.Fatalf("回收站里的项目保存场景应 40905，得到 %v", err)
	}
	_, _, err = svc.CreateDirectorScene(ctx, ws, pid, canvasapp.DirectorActor{UserID: 1}, collab.RoleOwner,
		canvasapp.CreateDirectorSceneInput{CanvasNodeID: "n_new"})
	if codeOf(err) != errcode.InvalidStateTransition {
		t.Fatalf("回收站里的项目建场景应 40905，得到 %v", err)
	}
	// 仓储层的底线：绕过 app 层直接写也写不进回收站里的项目。
	_, err = r.UpdateDirectorScene(ctx, ws, pid, sc.Scene.ID, 1, repo.DirectorSceneWrite{
		Scene: director.DefaultScene(), SchemaVersion: 1, RenderFingerprint: "x", Source: "user"})
	if !errors.Is(err, repo.ErrVersionConflict) {
		t.Fatalf("回收站里的项目仓储层应写不进（0 行），得到 %v", err)
	}
	if v, _ := r.GetDirectorScene(ctx, ws, pid, sc.Scene.ID); v.Version != 1 {
		t.Fatalf("版本不该变，得到 %d", v.Version)
	}
}

func TestDirectorScene_跨工作区404(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	p1 := mustInsert(t, r, next, ws, "项目一")
	p2 := mustInsert(t, r, next, ws, "项目二")
	sc := newScene(t, svc, ws, p1, "n_dir")

	// 别的工作区：项目本身就查不到。
	if _, err := svc.GetDirectorScene(ctx, ws+1, p1, sc.Scene.ID, collab.RoleOwner); codeOf(err) != errcode.NotFound {
		t.Fatalf("跨工作区读应 404，得到 %v", err)
	}
	// 同工作区的另一个项目：场景不属于它，也是 404（副本项目不能读到源项目那份）。
	if _, err := svc.GetDirectorScene(ctx, ws, p2, sc.Scene.ID, collab.RoleOwner); codeOf(err) != errcode.NotFound {
		t.Fatalf("场景不属于该项目应 404，得到 %v", err)
	}
	_, err := svc.UpdateDirectorScene(ctx, ws, p2, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 1, director.DefaultScene())
	if codeOf(err) != errcode.NotFound {
		t.Fatalf("往别的项目的场景里写应 404 而不是冲突，得到 %v", err)
	}
}

func TestDirectorScene_摘要不含正文(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	pid := mustInsert(t, r, next, ws, "项目")
	a := newScene(t, svc, ws, pid, "n_a")
	b := newScene(t, svc, ws, pid, "n_b")
	// b 去掉输出机位（null）。
	noOutput := editedScene(t, func(m map[string]any) { m["outputCameraId"] = nil })
	if _, err := svc.UpdateDirectorScene(ctx, ws, pid, b.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 1, noOutput); err != nil {
		t.Fatal(err)
	}

	list, err := svc.ListDirectorScenes(ctx, ws, pid, collab.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != a.Scene.ID || list[1].ID != b.Scene.ID {
		t.Fatalf("应按建立先后列出两个场景，得到 %+v", list)
	}
	var head struct {
		OutputCameraID string `json:"outputCameraId"`
	}
	_ = json.Unmarshal(director.DefaultScene(), &head)
	if list[0].OutputCameraID != head.OutputCameraID || head.OutputCameraID == "" {
		t.Errorf("摘要的输出机位应取正文里的 %q，得到 %q", head.OutputCameraID, list[0].OutputCameraID)
	}
	if list[1].OutputCameraID != "" || list[1].Version != 2 {
		t.Errorf("没有输出机位时摘要为空、版本为 2，得到 %+v", list[1])
	}
	if list[0].RenderFingerprint != a.Scene.RenderFingerprint {
		t.Errorf("摘要的指纹应与场景一致")
	}
}

// DR-AC-50：未知字段保存后读回原样；DR-AC-51：写入其他工作区的媒体被拒、库里版本不变。
func TestDirectorScene_未知字段与媒体归属(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	svc.SetMediaLookup(wsMedia{
		801: {WorkspaceID: ws, Kind: "image", Purpose: "generation_output"},
		802: {WorkspaceID: ws + 1, Kind: "image", Purpose: "generation_output"},
	})
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")

	panorama := func(mediaID string) func(m map[string]any) {
		return func(m map[string]any) {
			m["background"] = map[string]any{"mode": "panorama", "color": "#060608", "futureKey": []any{1, "二"},
				"panorama": map[string]any{"sourceNodeId": "n_pano", "mediaId": mediaID, "yaw": 90, "radius": 40}}
			m["futureTopLevel"] = map[string]any{"嵌套": map[string]any{"数组": []any{1.5, true, nil}}}
		}
	}
	good := editedScene(t, panorama("801"))
	saved, err := svc.UpdateDirectorScene(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 1, good)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.MediaIDs) != 1 || saved.MediaIDs[0] != "801" {
		t.Errorf("media_ids 应提取出全景图 801，得到 %v", saved.MediaIDs)
	}
	v, _ := svc.GetDirectorScene(ctx, ws, pid, sc.Scene.ID, collab.RoleViewer)
	if !sameJSON(t, v.Scene.Scene, good) {
		t.Fatalf("未知字段应原样读回，得到 %s", v.Scene.Scene)
	}

	_, err = svc.UpdateDirectorScene(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 2, editedScene(t, panorama("802")))
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.InvalidArgument || be.Details["path"] != "background.panorama.mediaId" {
		t.Fatalf("别的工作区的媒体应 40001 并指到字段，得到 %v", err)
	}
	if cur, _ := r.GetDirectorScene(ctx, ws, pid, sc.Scene.ID); cur.Version != 2 {
		t.Fatalf("被拒的保存不该推进版本，得到 %d", cur.Version)
	}
}
