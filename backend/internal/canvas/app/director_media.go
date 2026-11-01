package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	director "github.com/li904338203/tabtv/backend/internal/director/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 3D 导演台媒体上传（docs/agent/director，DRT-03）
//
// 截图 / 控制图、预演视频、泼溅文件都走导演台自己的上传端点（DRD-15）：purpose 记成导演台的三个值，
// 「我的素材」只列 upload_reference，它们天然不进去（DRK-11）。每个文件在 director_media 留一条记录（DRD-12）。

// DirectorMediaStore 由媒体域实现：上传与软删。画布域不 import 媒体域（同 MediaReleaser 的理由）。
type DirectorMediaStore interface {
	Upload(ctx context.Context, in DirectorUploadInput) (UploadedMedia, error)
	// Delete 软删一个媒体（先查引用再删，被别处引用时回 40906）。
	Delete(ctx context.Context, workspaceID, mediaID int64) error
}

// DirectorUploadInput 是一次上传。ExpectKind 取 image / video / splat。
type DirectorUploadInput struct {
	WorkspaceID  int64
	UserID       int64
	Purpose      string
	ExpectKind   string
	MaxBytes     int64
	DeclaredMIME string
	FileName     string
	Size         int64
	Body         io.Reader
}

// UploadedMedia 是上传结果。尺寸与时长 0 表示未知。
type UploadedMedia struct {
	ID         int64
	MIME       string
	Width      int
	Height     int
	DurationMs int
}

// DirectorFile 是请求里带的文件。
type DirectorFile struct {
	DeclaredMIME string
	FileName     string
	Size         int64
	Body         io.Reader
}

// DirectorUploadLimits 是两个可配置的上限（dev/01 §9，DR-PARAM-10 / 11）。0 = 用媒体域的默认上限。
type DirectorUploadLimits struct {
	PreviewMaxBytes int64
	SplatMaxBytes   int64
}

// 导演台的三个 purpose（DRD-15）与 media_object 的 CHECK 一致。刻意重写字面量（同 usageProjectCover）。
const (
	purposeDirectorCapture = "director_capture"
	purposeDirectorPreview = "director_preview"
)

// maxCameraSnapshotBytes 是记录上机位快照的上限：它只是焦距、位置、朝向与深度范围几项，
// 不设上限的话这个字段就成了一条往库里塞任意大 JSON 的口子。
const maxCameraSnapshotBytes = 16 << 10

// maxPreviewMs 是预演时长上限：时间轴最长 450 帧（DR-PARAM-10），多给一帧的余量。
const maxPreviewMs = director.MaxDurationFrames*1000/director.TimelineFPS + 1000/director.TimelineFPS

// SetDirectorMedia 接上导演台的媒体上传端口（wiring 用媒体域实现）。
func (s *Service) SetDirectorMedia(m DirectorMediaStore) { s.directorMedia = m }

// SetDirectorUploadLimits 接上两个可配置的上传上限。
func (s *Service) SetDirectorUploadLimits(l DirectorUploadLimits) { s.directorLimits = l }

// DirectorCaptureInput 是截图 / 控制图上传。
type DirectorCaptureInput struct {
	Kind         string // capture / depth / pose
	CameraID     string
	SceneVersion int
	// Camera 是前端给的机位快照（含深度图的 near / far，服务端没有这些），可省略。
	Camera json.RawMessage
	File   DirectorFile
}

// DirectorPreviewInput 是预演视频上传。
type DirectorPreviewInput struct {
	CameraID     string
	SceneVersion int
	DurationMs   int
	FPS          int
	Camera       json.RawMessage
	File         DirectorFile
}

// UploadDirectorCapture 上传截图 / 控制图（dev/01 §3.4）。
//
// ⚠️ **先核版本，再上传**（DRD-14）：不核的话 A 窗口的截图会盖上 B 窗口刚存的版本号，过期判定从此对不上；
// 先上传后核会留下孤立文件（DR-AC-18）。记录上的指纹取核版本时读到的那一份 —— 核完之后有人保存了的话，
// 这张截图本来就是旧画面，立即显示「过期」才是对的。
func (s *Service) UploadDirectorCapture(ctx context.Context, ws, projectID, sceneID int64, actor DirectorActor,
	role collab.Role, in DirectorCaptureInput) (repo.DirectorMedia, error) {
	switch in.Kind {
	case "capture", "depth", "pose":
	default:
		return repo.DirectorMedia{}, httpx.Errf(errcode.InvalidArgument, "kind 只能是 capture / depth / pose")
	}
	sc, cam, err := s.directorUploadTarget(ctx, ws, projectID, sceneID, actor, role, in.SceneVersion, in.CameraID)
	if err != nil {
		return repo.DirectorMedia{}, err
	}
	snapshot, err := cameraSnapshot(in.Camera)
	if err != nil {
		return repo.DirectorMedia{}, err
	}
	up, err := s.directorUpload(ctx, ws, actor, purposeDirectorCapture, "image", 0, in.File)
	if err != nil {
		return repo.DirectorMedia{}, err
	}
	return s.insertDirectorRecord(ctx, ws, projectID, actor, up, repo.DirectorMedia{
		SceneID: sc.ID, Kind: in.Kind, Width: positive(up.Width), Height: positive(up.Height),
		CameraID: strPtr(cam.ID()), CameraName: strPtr(cam.Name()), Camera: snapshot,
		SceneVersion: intPtr(sc.Version), RenderFingerprint: strPtr(sc.RenderFingerprint),
	})
}

// UploadDirectorPreview 上传预演视频（dev/01 §3.5）：与截图同样先核版本；上限取配置（只能收紧）。
//
// ⚠️ 时长以上传时探测到的为准（ffprobe）：与声明值相差超过 1 帧时拒绝 —— 声明值来自前端的时间轴，
// 编码器丢帧或截断时两者对不上，下游拿一段比时间轴短的视频当参考，镜头会对不上。
// 探不出时长（ffprobe 没装或认不出封装）时按声明值，并告警。
func (s *Service) UploadDirectorPreview(ctx context.Context, ws, projectID, sceneID int64, actor DirectorActor,
	role collab.Role, in DirectorPreviewInput) (repo.DirectorMedia, error) {
	if in.FPS < 1 || in.FPS > 60 {
		return repo.DirectorMedia{}, httpx.Errf(errcode.InvalidArgument, "fps 必须在 1~60 之间")
	}
	if in.DurationMs < 1 || in.DurationMs > maxPreviewMs {
		return repo.DirectorMedia{}, httpx.Errf(errcode.InvalidArgument, "durationMs 超出时间轴的时长范围")
	}
	sc, cam, err := s.directorUploadTarget(ctx, ws, projectID, sceneID, actor, role, in.SceneVersion, in.CameraID)
	if err != nil {
		return repo.DirectorMedia{}, err
	}
	snapshot, err := cameraSnapshot(in.Camera)
	if err != nil {
		return repo.DirectorMedia{}, err
	}
	up, err := s.directorUpload(ctx, ws, actor, purposeDirectorPreview, "video", s.directorLimits.PreviewMaxBytes, in.File)
	if err != nil {
		return repo.DirectorMedia{}, err
	}
	duration := in.DurationMs
	if up.DurationMs > 0 {
		if math.Abs(float64(up.DurationMs-in.DurationMs)) > 1000/float64(in.FPS) {
			s.dropUploaded(ctx, ws, up.ID)
			return repo.DirectorMedia{}, httpx.Errf(errcode.InvalidArgument,
				"预演视频实际时长 "+itoa(int64(up.DurationMs))+" 毫秒，与声明的 "+itoa(int64(in.DurationMs))+
					" 毫秒相差超过 1 帧，请重新导出")
		}
		duration = up.DurationMs
	} else {
		s.logWarn(ctx, "预演视频探不出时长，按声明值记录", projectID, errors.New("ffprobe 未给出时长"))
	}
	return s.insertDirectorRecord(ctx, ws, projectID, actor, up, repo.DirectorMedia{
		SceneID: sc.ID, Kind: "preview", Width: positive(up.Width), Height: positive(up.Height),
		DurationMs: intPtr(duration), FPS: intPtr(in.FPS),
		CameraID: strPtr(cam.ID()), CameraName: strPtr(cam.Name()), Camera: snapshot,
		SceneVersion: intPtr(sc.Version), RenderFingerprint: strPtr(sc.RenderFingerprint),
	})
}

// UploadDirectorAsset 上传泼溅文件（dev/01 §3.5、DRD-28）。**不核场景版本**：上传后由前端写进场景再保存。
// 上传了却始终没被场景引用的文件，清理项目时随记录一起回收（DRT-04）。
func (s *Service) UploadDirectorAsset(ctx context.Context, ws, projectID, sceneID int64, actor DirectorActor,
	role collab.Role, kind string, file DirectorFile) (repo.DirectorMedia, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.DirectorMedia{}, err
	}
	if kind != "splat" {
		return repo.DirectorMedia{}, httpx.Errf(errcode.InvalidArgument, "kind 只能是 splat")
	}
	if err := s.projectWritable(ctx, ws, projectID); err != nil {
		return repo.DirectorMedia{}, directorProjectErr(err)
	}
	sc, err := s.repo.GetDirectorScene(ctx, ws, projectID, sceneID)
	if err != nil {
		return repo.DirectorMedia{}, wrapDirectorRepo(err)
	}
	up, err := s.directorUpload(ctx, ws, actor, purposeDirectorAsset, "splat", s.directorLimits.SplatMaxBytes, file)
	if err != nil {
		return repo.DirectorMedia{}, err
	}
	return s.insertDirectorRecord(ctx, ws, projectID, actor, up, repo.DirectorMedia{SceneID: sc.ID, Kind: "splat"})
}

// SetDirectorRecordHidden 只改隐藏标记（DR-F-75）：文件与删除保护都不受影响。
func (s *Service) SetDirectorRecordHidden(ctx context.Context, ws, projectID, sceneID, recordID int64,
	role collab.Role, hidden bool) (repo.DirectorMedia, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.DirectorMedia{}, err
	}
	if err := s.projectWritable(ctx, ws, projectID); err != nil {
		return repo.DirectorMedia{}, directorProjectErr(err)
	}
	m, err := s.repo.SetDirectorMediaHidden(ctx, ws, projectID, sceneID, recordID, hidden)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.DirectorMedia{}, httpx.Errf(errcode.NotFound, "记录不存在")
	}
	if err != nil {
		return repo.DirectorMedia{}, httpx.Wrap(errcode.Internal, err)
	}
	return m, nil
}

// directorUploadTarget 是截图与预演的共同前置：权限 → 版本 → 项目可写 → 读场景 → 核版本 → 找机位。
// 全部在上传之前做完：任何一步不通过，文件都不该进对象存储。
func (s *Service) directorUploadTarget(ctx context.Context, ws, projectID, sceneID int64, actor DirectorActor,
	role collab.Role, sceneVersion int, cameraID string) (repo.DirectorScene, director.Object, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.DirectorScene{}, director.Object{}, err
	}
	if sceneVersion < 1 {
		return repo.DirectorScene{}, director.Object{}, httpx.Errf(errcode.InvalidArgument,
			"sceneVersion 必须来自服务端，不能为 0")
	}
	if strings.TrimSpace(cameraID) == "" {
		return repo.DirectorScene{}, director.Object{}, httpx.Errf(errcode.InvalidArgument, "cameraId 必填")
	}
	if err := s.projectWritable(ctx, ws, projectID); err != nil {
		return repo.DirectorScene{}, director.Object{}, directorProjectErr(err)
	}
	sc, err := s.repo.GetDirectorScene(ctx, ws, projectID, sceneID)
	if err != nil {
		return repo.DirectorScene{}, director.Object{}, wrapDirectorRepo(err)
	}
	if sc.Version != sceneVersion {
		return repo.DirectorScene{}, director.Object{}, httpx.Errf(errcode.VersionConflict,
			"场景已更新到第 "+itoa(int64(sc.Version))+" 版，截图对应的不是当前画面，请重新截图").
			WithDetails(directorConflictDetails(sc, actor))
	}
	parsed, err := director.ParseScene(sc.Scene)
	if err != nil {
		return repo.DirectorScene{}, director.Object{}, httpx.Wrap(errcode.Internal, err)
	}
	cam, ok := parsed.ObjectByID(cameraID)
	if !ok || cam.Kind() != director.KindCamera {
		return repo.DirectorScene{}, director.Object{}, httpx.Errf(errcode.InvalidArgument,
			"机位不存在："+cameraID).WithDetails(map[string]any{"path": "cameraId"})
	}
	return sc, cam, nil
}

func (s *Service) directorUpload(ctx context.Context, ws int64, actor DirectorActor, purpose, expect string,
	maxBytes int64, f DirectorFile) (UploadedMedia, error) {
	if s.directorMedia == nil {
		return UploadedMedia{}, httpx.Errf(errcode.Internal, "导演台媒体上传未接入")
	}
	return s.directorMedia.Upload(ctx, DirectorUploadInput{
		WorkspaceID: ws, UserID: actor.UserID, Purpose: purpose, ExpectKind: expect, MaxBytes: maxBytes,
		DeclaredMIME: f.DeclaredMIME, FileName: f.FileName, Size: f.Size, Body: f.Body,
	})
}

// insertDirectorRecord 插记录；失败时软删刚上传的文件。
//
// ⚠️ 不删的话这个文件就成了孤儿：purpose 是隐藏的，「我的素材」里看不到，用户永远删不到它（DRD-30 同理）。
func (s *Service) insertDirectorRecord(ctx context.Context, ws, projectID int64, actor DirectorActor,
	up UploadedMedia, m repo.DirectorMedia) (repo.DirectorMedia, error) {
	m.ID, m.WorkspaceID, m.ProjectID, m.MediaID, m.CreatedByUserID = s.nextID(), ws, projectID, up.ID, actor.UserID
	rec, err := s.repo.InsertDirectorMedia(ctx, m)
	if err == nil {
		return rec, nil
	}
	s.dropUploaded(ctx, ws, up.ID)
	if errors.Is(err, repo.ErrNotFound) {
		// 上传期间项目被删进回收站（或场景所在项目已不可写）：说清是哪一种。
		if werr := s.projectWritable(ctx, ws, projectID); werr != nil {
			return repo.DirectorMedia{}, directorProjectErr(werr)
		}
		return repo.DirectorMedia{}, httpx.Errf(errcode.NotFound, "场景不存在")
	}
	return repo.DirectorMedia{}, httpx.Wrap(errcode.Internal, err)
}

// dropUploaded 尽力软删一个刚上传、却没能挂上记录的文件；失败只告警（媒体回收队列不会收它，要人看见）。
func (s *Service) dropUploaded(ctx context.Context, ws, mediaID int64) {
	if s.directorMedia == nil {
		return
	}
	if err := s.directorMedia.Delete(ctx, ws, mediaID); err != nil && s.log != nil {
		s.log.WarnContext(ctx, "导演台上传失败后软删文件失败，留下了一个孤立文件", "mediaId", mediaID, "error", err)
	}
}

// cameraSnapshot 校验前端给的机位快照：可省略；给了就必须是 JSON 对象，且不超过上限。
func cameraSnapshot(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if len(trimmed) > maxCameraSnapshotBytes {
		return nil, httpx.Errf(errcode.InvalidArgument, "camera 快照过大").WithDetails(map[string]any{"path": "camera"})
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil || obj == nil {
		return nil, httpx.Errf(errcode.InvalidArgument, "camera 必须是 JSON 对象").WithDetails(map[string]any{"path": "camera"})
	}
	return json.RawMessage(trimmed), nil
}

func positive(v int) *int {
	if v <= 0 {
		return nil
	}
	return &v
}

func intPtr(v int) *int { return &v }

func strPtr(v string) *string { return &v }
