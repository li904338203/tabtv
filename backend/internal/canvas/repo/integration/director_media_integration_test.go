//go:build integration

// 3D 导演台媒体记录与删除保护（docs/agent/director，DRT-03）的真库测试。
//
// 上传编排的用例也放在这里（卡片写的是单测）：编排的前置要读场景当前版本、核项目是否可写，
// 而画布 app 的仓储是具体类型，没有可替换的接口 —— 只能接真库，上传端口用假的。
package integration

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// fakeDirectorStore 记下上传与删除的调用；不碰对象存储。
type fakeDirectorStore struct {
	mu       sync.Mutex
	next     func() int64
	uploads  []canvasapp.DirectorUploadInput
	deleted  []int64
	duration int
	// onUpload 在上传「进行中」执行（模拟上传期间项目被删进回收站）。
	onUpload func()
}

func (f *fakeDirectorStore) Upload(_ context.Context, in canvasapp.DirectorUploadInput) (canvasapp.UploadedMedia, error) {
	f.mu.Lock()
	f.uploads = append(f.uploads, in)
	f.mu.Unlock()
	if f.onUpload != nil {
		f.onUpload()
	}
	return canvasapp.UploadedMedia{ID: f.next(), MIME: "image/png", Width: 1920, Height: 1080,
		DurationMs: f.duration}, nil
}

func (f *fakeDirectorStore) Delete(_ context.Context, _, mediaID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, mediaID)
	return nil
}

func file(content string) canvasapp.DirectorFile {
	return canvasapp.DirectorFile{FileName: "shot.png", Size: int64(len(content)), Body: bytes.NewReader([]byte(content))}
}

// defaultCameraID 是默认场景里那台机位的 ID。
func defaultCameraID(t *testing.T) string {
	t.Helper()
	sc, err := director.ParseScene(director.DefaultScene())
	if err != nil {
		t.Fatal(err)
	}
	return sc.OutputCameraID()
}

func directorSetup(t *testing.T) (*canvasapp.Service, *repo.Repo, *fakeDirectorStore, func() int64) {
	t.Helper()
	svc, r, _, store, next := directorSetupPool(t)
	return svc, r, store, next
}

func directorSetupPool(t *testing.T) (*canvasapp.Service, *repo.Repo, *pgxpool.Pool, *fakeDirectorStore, func() int64) {
	t.Helper()
	r, pool, next := setup(t)
	svc := canvasapp.New(r, next)
	store := &fakeDirectorStore{next: next}
	svc.SetDirectorMedia(store)
	return svc, r, pool, store, next
}

func captureIn(t *testing.T, version int) canvasapp.DirectorCaptureInput {
	return canvasapp.DirectorCaptureInput{Kind: "capture", CameraID: defaultCameraID(t), SceneVersion: version,
		Camera: []byte(`{"focalLengthMm":35}`), File: file("png")}
}

func TestUploadDirectorCapture_旧版本409不上传(t *testing.T) {
	svc, r, store, next := directorSetup(t)
	ctx := context.Background()
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")
	if _, err := svc.UpdateDirectorScene(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 1, director.DefaultScene()); err != nil {
		t.Fatal(err)
	}

	_, err := svc.UploadDirectorCapture(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, captureIn(t, 1))
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.VersionConflict || be.Details["currentVersion"] != 2 {
		t.Fatalf("旧版本截图应 40902 并带当前版本 2，得到 %v", err)
	}
	// DR-AC-18：不上传文件、不新增记录。
	if len(store.uploads) != 0 {
		t.Fatalf("版本不一致时不该上传文件，却上传了 %d 次", len(store.uploads))
	}
	if list, _ := r.ListDirectorMedia(ctx, ws, pid, sc.Scene.ID); len(list) != 0 {
		t.Fatalf("不该新增记录，得到 %d 条", len(list))
	}
}

func TestUploadDirectorCapture_机位不存在拒绝(t *testing.T) {
	svc, r, store, next := directorSetup(t)
	ctx := context.Background()
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")
	in := captureIn(t, 1)
	in.CameraID = "obj_not_here"
	_, err := svc.UploadDirectorCapture(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, in)
	if codeOf(err) != errcode.InvalidArgument || len(store.uploads) != 0 {
		t.Fatalf("机位不存在应 40001 且不上传，得到 %v（上传 %d 次）", err, len(store.uploads))
	}
	// kind 非法、只读成员：同样不上传。
	in = captureIn(t, 1)
	in.Kind = "thumbnail"
	if _, err := svc.UploadDirectorCapture(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, in); codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("kind 非法应 40001，得到 %v", err)
	}
	if _, err := svc.UploadDirectorCapture(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleViewer, captureIn(t, 1)); codeOf(err) != errcode.PermissionDenied {
		t.Fatalf("viewer 截图应 403，得到 %v", err)
	}
	if len(store.uploads) != 0 {
		t.Fatalf("被拒的请求都不该上传，却上传了 %d 次", len(store.uploads))
	}
}

func TestUploadDirectorCapture_成功留记录(t *testing.T) {
	svc, r, store, next := directorSetup(t)
	ctx := context.Background()
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")
	rec, err := svc.UploadDirectorCapture(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 7},
		collab.RoleEditor, captureIn(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	if store.uploads[0].Purpose != "director_capture" || store.uploads[0].ExpectKind != "image" {
		t.Fatalf("截图应以 director_capture / image 上传，得到 %+v", store.uploads[0])
	}
	if rec.Kind != "capture" || *rec.SceneVersion != 1 || *rec.RenderFingerprint != sc.Scene.RenderFingerprint ||
		*rec.CameraID != defaultCameraID(t) || rec.CameraName == nil || *rec.CameraName == "" ||
		*rec.Width != 1920 || *rec.Height != 1080 || rec.Hidden || rec.CreatedByUserID != 7 {
		t.Fatalf("记录内容不对：%+v", rec)
	}
	if !strings.Contains(string(rec.Camera), "focalLengthMm") {
		t.Errorf("机位快照应原样记下，得到 %s", rec.Camera)
	}
	// GET 带上记录；隐藏只改标记。
	v, _ := svc.GetDirectorScene(ctx, ws, pid, sc.Scene.ID, collab.RoleViewer)
	if len(v.Records) != 1 || v.Records[0].ID != rec.ID {
		t.Fatalf("GET 应带上记录，得到 %+v", v.Records)
	}
	hidden, err := svc.SetDirectorRecordHidden(ctx, ws, pid, sc.Scene.ID, rec.ID, collab.RoleEditor, true)
	if err != nil || !hidden.Hidden || hidden.MediaID != rec.MediaID {
		t.Fatalf("隐藏应只改标记：%+v %v", hidden, err)
	}
	v, _ = svc.GetDirectorScene(ctx, ws, pid, sc.Scene.ID, collab.RoleViewer)
	if len(v.Records) != 1 || !v.Records[0].Hidden {
		t.Fatalf("隐藏的记录仍要在列表里（带 hidden 标记），得到 %+v", v.Records)
	}
	if _, err := svc.SetDirectorRecordHidden(ctx, ws, pid, sc.Scene.ID, 424242, collab.RoleEditor, true); codeOf(err) != errcode.NotFound {
		t.Fatalf("不存在的记录应 404，得到 %v", err)
	}
}

func TestUploadDirectorCapture_插记录失败软删文件(t *testing.T) {
	svc, r, store, next := directorSetup(t)
	ctx := context.Background()
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")
	// 上传期间项目被删进回收站：记录插不进去，刚上传的文件必须软删（purpose 隐藏，用户永远删不到它）。
	store.onUpload = func() {
		if err := r.SoftDelete(ctx, ws, pid); err != nil {
			t.Error(err)
		}
	}
	_, err := svc.UploadDirectorCapture(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, captureIn(t, 1))
	if codeOf(err) != errcode.InvalidStateTransition {
		t.Fatalf("上传期间项目进了回收站应 40905，得到 %v", err)
	}
	if len(store.uploads) != 1 || len(store.deleted) != 1 {
		t.Fatalf("应上传 1 次并软删 1 次，得到上传 %d、软删 %d", len(store.uploads), len(store.deleted))
	}
	if list, _ := r.ListDirectorMedia(ctx, ws, pid, sc.Scene.ID); len(list) != 0 {
		t.Fatalf("不该留下记录，得到 %d 条", len(list))
	}
}

func TestUploadDirectorPreview_时长与声明不符拒绝(t *testing.T) {
	svc, r, store, next := directorSetup(t)
	ctx := context.Background()
	svc.SetDirectorUploadLimits(canvasapp.DirectorUploadLimits{PreviewMaxBytes: 50 << 20})
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")
	in := canvasapp.DirectorPreviewInput{CameraID: defaultCameraID(t), SceneVersion: 1, DurationMs: 5000, FPS: 30,
		File: file("mp4")}

	store.duration = 4000 // 探测到 4 秒，声明 5 秒：差了 30 帧
	_, err := svc.UploadDirectorPreview(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, in)
	if codeOf(err) != errcode.InvalidArgument || len(store.deleted) != 1 {
		t.Fatalf("时长差超过 1 帧应 40001 并删掉文件，得到 %v（软删 %d）", err, len(store.deleted))
	}
	if store.uploads[0].Purpose != "director_preview" || store.uploads[0].ExpectKind != "video" ||
		store.uploads[0].MaxBytes != 50<<20 {
		t.Fatalf("预演应以 director_preview / video / 配置上限上传，得到 %+v", store.uploads[0])
	}

	store.duration = 5020 // 差 20 毫秒，不到 1 帧（33 毫秒）：以探测值为准
	in.File = file("mp4")
	rec, err := svc.UploadDirectorPreview(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, in)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Kind != "preview" || *rec.DurationMs != 5020 || *rec.FPS != 30 {
		t.Fatalf("时长以探测值为准：%+v", rec)
	}

	store.duration = 0 // 探不出时长：按声明值
	in.File = file("mp4")
	rec, err = svc.UploadDirectorPreview(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, in)
	if err != nil || *rec.DurationMs != 5000 {
		t.Fatalf("探不出时长时按声明值：%+v %v", rec, err)
	}
}

func TestUploadDirectorAsset_泼溅不核版本(t *testing.T) {
	svc, r, store, next := directorSetup(t)
	ctx := context.Background()
	svc.SetDirectorUploadLimits(canvasapp.DirectorUploadLimits{SplatMaxBytes: 100 << 20})
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")
	rec, err := svc.UploadDirectorAsset(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, "splat", file("ply"))
	if err != nil {
		t.Fatal(err)
	}
	if store.uploads[0].Purpose != "director_asset" || store.uploads[0].ExpectKind != "splat" {
		t.Fatalf("泼溅应以 director_asset / splat 上传，得到 %+v", store.uploads[0])
	}
	if rec.Kind != "splat" || rec.SceneVersion != nil || rec.RenderFingerprint != nil || rec.CameraID != nil {
		t.Fatalf("泼溅记录不带版本、指纹与机位：%+v", rec)
	}
	if _, err := svc.UploadDirectorAsset(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, "mesh", file("x")); codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("kind 只能是 splat，得到 %v", err)
	}
}

func TestDirectorMediaReferences_场景正文与记录都算(t *testing.T) {
	svc, r, pool, _, next := directorSetupPool(t)
	ctx := context.Background()
	pano, captured := next(), int64(0)
	svc.SetMediaLookup(wsMedia{pano: {WorkspaceID: ws, Kind: "image"}})
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")
	body := editedScene(t, func(m map[string]any) {
		m["background"] = map[string]any{"mode": "panorama",
			"panorama": map[string]any{"sourceNodeId": "n_p", "mediaId": itoa10(pano)}}
	})
	if _, err := svc.UpdateDirectorScene(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleOwner, 1, body); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.UploadDirectorCapture(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, captureIn(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	captured = rec.MediaID

	for name, id := range map[string]int64{"正文里的全景": pano, "截图记录": captured} {
		refs, err := r.DirectorMediaReferences(ctx, id)
		if err != nil || len(refs) != 1 || refs[0] != sc.Scene.ID {
			t.Errorf("%s应被场景引用，得到 %v %v", name, refs, err)
		}
	}
	if refs, _ := r.DirectorMediaReferences(ctx, 424242); len(refs) != 0 {
		t.Errorf("没被引用的媒体应为空，得到 %v", refs)
	}

	// DR-AC-44：经媒体域删除时回 40906，文案点名「3D 导演台」。
	media := mediaapp.New(mediarepo.New(pool), nil, next)
	media.AddReferenceChecker(refChecker{mediaapp.UsageDirectorScene, r.DirectorMediaReferences})
	err = media.Delete(ctx, ws, pano)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.ResourceInUse || !strings.Contains(be.Message, "3D 导演台") {
		t.Fatalf("删除被场景引用的全景应 40906 并点名 3D 导演台，得到 %v", err)
	}
}

func TestDirectorMediaReferences_含回收站项目(t *testing.T) {
	svc, r, _, next := directorSetup(t)
	ctx := context.Background()
	pid := mustInsert(t, r, next, ws, "项目")
	sc := newScene(t, svc, ws, pid, "n_dir")
	rec, err := svc.UploadDirectorCapture(ctx, ws, pid, sc.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, captureIn(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SoftDelete(ctx, ws, pid); err != nil {
		t.Fatal(err)
	}
	// 「删项目 → 删截图 → 恢复项目」会得到裂图：回收站里的项目照样算引用。
	refs, err := r.DirectorMediaReferences(ctx, rec.MediaID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("回收站里的项目仍算引用，得到 %v %v", refs, err)
	}
}

func TestDirectorMedia_不进我的素材(t *testing.T) {
	_, _, pool, _, next := directorSetupPool(t)
	ctx := context.Background()
	mr := mediarepo.New(pool)
	purposes := []string{"upload_reference", "director_capture", "director_preview", "director_asset"}
	ids := map[string]int64{}
	for _, p := range purposes {
		id := next()
		ids[p] = id
		if err := mr.Insert(ctx, mediarepo.MediaObject{ID: id, WorkspaceID: ws, Bucket: "b", ObjectKey: p + "/" + itoa10(id),
			ContentType: "image/png", SizeBytes: 1, Purpose: p, CreatedByUserID: 1}); err != nil {
			t.Fatalf("写入 %s 失败（purpose 的 CHECK 要含导演台的三个值）：%v", p, err)
		}
	}
	list, err := mr.ListByWorkspace(ctx, ws, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != ids["upload_reference"] {
		t.Fatalf("「我的素材」只列 upload_reference（DR-AC-45），得到 %+v", list)
	}
}

func TestDirectorScene_分叉复制媒体记录(t *testing.T) {
	svc, r, _, next := directorSetup(t)
	ctx := context.Background()
	pid := mustInsert(t, r, next, ws, "项目")
	src := newScene(t, svc, ws, pid, "n_src")
	rec, err := svc.UploadDirectorCapture(ctx, ws, pid, src.Scene.ID, canvasapp.DirectorActor{UserID: 1},
		collab.RoleEditor, captureIn(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetDirectorRecordHidden(ctx, ws, pid, src.Scene.ID, rec.ID, collab.RoleEditor, true); err != nil {
		t.Fatal(err)
	}
	fork, created, err := svc.CreateDirectorScene(ctx, ws, pid, canvasapp.DirectorActor{UserID: 1}, collab.RoleEditor,
		canvasapp.CreateDirectorSceneInput{CanvasNodeID: "n_copy", SourceSceneID: src.Scene.ID})
	if err != nil || !created {
		t.Fatalf("分叉应新建：%v", err)
	}
	if len(fork.Records) != 1 {
		t.Fatalf("分叉应带上源场景的记录，得到 %d 条", len(fork.Records))
	}
	c := fork.Records[0]
	if c.ID == rec.ID || c.SceneID != fork.Scene.ID || c.MediaID != rec.MediaID || !c.Hidden ||
		*c.RenderFingerprint != *rec.RenderFingerprint || !c.CreatedAt.Equal(rec.CreatedAt) {
		t.Fatalf("复制的记录：新 ID、指向新场景、媒体与快照不变，得到 %+v（源 %+v）", c, rec)
	}
	// 同一份文件被两个场景引用：删除保护照常成立。
	if refs, _ := r.DirectorMediaReferences(ctx, rec.MediaID); len(refs) != 2 {
		t.Fatalf("复制之后应有两个场景引用这份截图，得到 %v", refs)
	}
}

// refChecker 把仓储的引用查询接成媒体域的检查器（同两个 main 里的 mediaRefChecker）。
type refChecker struct {
	kind string
	fn   func(context.Context, int64) ([]int64, error)
}

func (c refChecker) Kind() string { return c.kind }
func (c refChecker) References(ctx context.Context, id int64) ([]int64, error) {
	return c.fn(ctx, id)
}
