package wiring

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // 素材图片可能是 GIF
	"image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // 素材图片可能是 WebP

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	providerapp "github.com/li904338203/tabtv/backend/internal/provider/app"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	smartcutrepo "github.com/li904338203/tabtv/backend/internal/smartcut/repo"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

// AI 剪辑的端口适配（docs/agent/smartcut SCT-27）：目录、Provider、时间线、生成与媒体都接成 smartcut/app 的端口。
// smartcut/app 不 import 它们任何一个（谁定义端口谁拥有契约），适配器写在组装层。

// SmartcutPlanInput 是 SmartcutPlanPorts 的入参。
type SmartcutPlanInput struct {
	Pool        *pgxpool.Pool
	Timeline    *timelineapp.Service
	Catalog     *catalogapp.Service
	Media       *mediaapp.Service
	Generations *genrepo.Repo
	Text        *providerapp.HTTPProvider
	Events      smartcutapp.PlanEvents
	// Board 分镜成片的装配（SCT-28）。为 nil 时分镜成片回 42217 no_shots。
	Board smartcutapp.BoardShots
	// RenderTimeline 把时间线渲染成对外形状（契约 Timeline）：由 cmd/api 传入时间线传输层的 ViewJSON ——
	// wiring 不依赖 HTTP 层（imports_test），而结果里的时间线必须与 GET /timelines/{id} 同一份渲染。
	RenderTimeline func(timelineapp.View) ([]byte, error)
	Cfg            config.Smartcut
	NextID         func() int64
	Logger         *slog.Logger
}

// SmartcutPlanPorts 装出 AI 剪辑的端口。集成测试换掉模型与目录（Text / Catalog），其余仍走真实的写入路径。
func SmartcutPlanPorts(in SmartcutPlanInput) smartcutapp.PlanPorts {
	p := smartcutapp.PlanPorts{
		Calls:     smartcutrepo.New(in.Pool),
		Timelines: SmartcutPlanTimelines{Svc: in.Timeline, Render: in.RenderTimeline},
		Material:  smartcutMaterial{gens: in.Generations, media: in.Media},
		Board:     in.Board,
		NextID:    in.NextID,
		Log:       in.Logger,
		Config: smartcutapp.PlanConfig{
			Concurrency: in.Cfg.PlanConcurrency, PerHour: in.Cfg.PlanPerHour, CallTimeout: in.Cfg.PlanTimeout,
			VisionTimeout: in.Cfg.PlanVisionTimeout, StaleAfter: in.Cfg.PlanStaleAfter,
		},
	}
	if in.Catalog != nil {
		p.Catalog, p.Prompts = SmartcutPlanCatalog{Svc: in.Catalog}, in.Catalog
	}
	if in.Text != nil {
		p.Text = SmartcutText{P: in.Text}
	}
	if in.Events != nil {
		p.Events = in.Events
	}
	return p
}

// SmartcutPlanCatalog 目录域的 smartcut_plan 选模型。
type SmartcutPlanCatalog struct{ Svc *catalogapp.Service }

func (a SmartcutPlanCatalog) SmartcutPlanChoice(ctx context.Context, memberLevelID int64, imageCount int) (
	smartcutapp.PlanChoice, *smartcutapp.Unavailable, error) {
	c, u, err := a.Svc.SmartcutPlanChoice(ctx, memberLevelID, imageCount)
	if err != nil || u != nil {
		if u != nil {
			return smartcutapp.PlanChoice{}, &smartcutapp.Unavailable{Reason: u.Reason, UserFixable: u.UserFixable}, err
		}
		return smartcutapp.PlanChoice{}, nil, err
	}
	return smartcutapp.PlanChoice{ModelCode: c.ModelCode, MaxImages: c.MaxImages}, nil, nil
}

// SmartcutText 文字模型的唯一入口（provider/app.CompleteText）。
type SmartcutText struct{ P *providerapp.HTTPProvider }

func (a SmartcutText) Complete(ctx context.Context, in smartcutapp.TextRequest) (string, error) {
	return a.P.CompleteText(ctx, providerapp.TextInput{ModelCode: in.ModelCode, System: in.System, User: in.User,
		Images: in.Images, Operation: in.Operation})
}

// SmartcutPlanTimelines 时间线域的读与整份写入：AI 落地走整份保存的同一条写路径（校验、素材归属、快照、乐观锁、
// timeline.updated 都在那里，SCD-19）。
type SmartcutPlanTimelines struct {
	Svc    *timelineapp.Service
	Render func(timelineapp.View) ([]byte, error)
}

func (a SmartcutPlanTimelines) Content(ctx context.Context, ws, timelineID int64) (timelinedomain.Content, error) {
	v, err := a.Svc.Get(ctx, ws, timelineID)
	if err != nil {
		return timelinedomain.Content{}, err
	}
	return timelineapp.ContentFromView(v), nil
}

func (a SmartcutPlanTimelines) Save(ctx context.Context, in smartcutapp.SavePlanInput) (smartcutapp.SavedPlan, error) {
	// 服务端拼出来的内容同样要过整份保存的校验（整份保存的入口在传输层解码时校验，这里不经过它）：
	// 编译器有 bug 时宁可报一个字段路径，也不把一份坏内容写进库。
	if raw, err := json.Marshal(in.Content); err != nil {
		return smartcutapp.SavedPlan{}, err
	} else if err := timelinedomain.ValidateContent(raw); err != nil {
		return smartcutapp.SavedPlan{}, timelineapp.ContentError(err)
	}
	save := timelineapp.SaveContentInput{WorkspaceID: in.WorkspaceID, TimelineID: in.TimelineID,
		Editor: timelinerepo.Editor{UserID: in.Editor.UserID, Kind: in.Editor.Kind}, Content: in.Content,
		Reason: in.Reason}
	if in.SnapshotReason != "" {
		save.Snapshot = &timelineapp.SnapshotSpec{Reason: in.SnapshotReason, CallID: in.CallID}
	}
	if in.Fingerprint != "" {
		save.Fingerprints = &timelinerepo.FingerprintWrite{Synced: in.Fingerprint, Planned: true, AfterPlan: true}
	}
	res, err := a.Svc.SaveContent(ctx, save)
	if err != nil {
		return smartcutapp.SavedPlan{}, err
	}
	out := smartcutapp.SavedPlan{Version: res.View.Timeline.Version, SnapshotID: res.SnapshotID, IDMap: res.IDMap}
	if a.Render != nil {
		raw, err := a.Render(res.View)
		if err != nil {
			return smartcutapp.SavedPlan{}, err
		}
		out.Timeline = raw
	}
	return out, nil
}

func (a SmartcutPlanTimelines) Conflict(ctx context.Context, timelineID int64, ed smartcutapp.Editor) error {
	return a.Svc.ConflictError(ctx, timelineID, timelinerepo.Editor{UserID: ed.UserID, Kind: ed.Kind})
}

// ---------------------------------------------------------------- 素材事实

type smartcutMaterial struct {
	gens  *genrepo.Repo
	media *mediaapp.Service
}

func (a smartcutMaterial) Descriptions(ctx context.Context, ws, _ int64, ids []int64, model string) (map[int64]string, error) {
	out := map[int64]string{}
	if a.gens != nil {
		prompts, err := a.gens.PromptsByOutputMedia(ctx, ws, ids)
		if err != nil {
			return nil, err
		}
		for id, p := range prompts {
			out[id] = p
		}
	}
	if model != "" && a.media != nil {
		// 这个模型写过的看图描述优先：它是看着画面写的，比生成提示词更贴近成品。
		caps, err := a.media.AnalysisResults(ctx, ws, ids, "caption", model)
		if err != nil {
			return nil, err
		}
		for id, raw := range caps {
			var c struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(raw, &c) == nil && strings.TrimSpace(c.Text) != "" {
				out[id] = c.Text
			}
		}
	}
	return out, nil
}

func (a smartcutMaterial) Beats(ctx context.Context, ws int64, ids []int64) (map[int64]smartcutdomain.Beats, error) {
	out := map[int64]smartcutdomain.Beats{}
	rows, err := a.media.AnalysisResults(ctx, ws, ids, "beats", "")
	if err != nil {
		return nil, err
	}
	for id, raw := range rows {
		var b smartcutdomain.Beats
		if json.Unmarshal(raw, &b) == nil && len(b.BeatsMs) > 0 {
			out[id] = b
		}
	}
	return out, nil
}

func (a smartcutMaterial) Peaks(ctx context.Context, ws int64, ids []int64) (map[int64]smartcutdomain.Peaks, error) {
	out := map[int64]smartcutdomain.Peaks{}
	rows, err := a.media.AnalysisResults(ctx, ws, ids, "peaks", "")
	if err != nil {
		return nil, err
	}
	for id, raw := range rows {
		var p mediaapp.Peaks
		if json.Unmarshal(raw, &p) != nil || p.NoAudio || p.IntervalMs <= 0 {
			continue
		}
		vals, err := base64.StdEncoding.DecodeString(p.Values)
		if err != nil || len(vals) == 0 {
			continue
		}
		out[id] = smartcutdomain.Peaks{IntervalMs: p.IntervalMs, Values: vals}
	}
	return out, nil
}

// Transcripts 按用途挑识别结果（最新的优先）：口播要保留口癖（参数键 f1.*）且有字级或词级时间。
// 挑不出来的给出原因 —— 「识别过但不能用」与「没识别过」要分开说，前者要换参数重新识别。
func (a smartcutMaterial) Transcripts(ctx context.Context, ws int64, ids []int64, need smartcutapp.TranscriptNeed) (
	map[int64]smartcutdomain.Transcript, map[int64]string, error) {
	out := map[int64]smartcutdomain.Transcript{}
	why := map[int64]string{}
	rows, err := a.media.ASRSuccessRows(ctx, ws, ids)
	if err != nil {
		return nil, nil, err
	}
	// 一份素材可能有几份成功的识别（换了模型、换了参数）：最新的在前，用哪一份按用途挑（规则在智能剪辑域）。
	cands := map[int64][]smartcutapp.TranscriptCandidate{}
	for _, row := range rows {
		cands[row.MediaObjectID] = append(cands[row.MediaObjectID],
			smartcutapp.TranscriptCandidate{ParamsKey: row.ParamsKey, Result: row.Result})
	}
	for id, list := range cands {
		if tr, reason, ok := smartcutapp.PickTranscript(list, need); ok {
			out[id] = tr
		} else if reason != "" {
			why[id] = reason
		}
	}
	return out, why, nil
}

// VoiceScripts 配音的原稿：只认文字转语音产出的音频，按那次生成的输入快照还原（规则在 smartcutdomain.SpokenScript）。
func (a smartcutMaterial) VoiceScripts(ctx context.Context, ws int64, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if a.gens == nil || len(ids) == 0 {
		return out, nil
	}
	srcs, err := a.gens.SourcesByOutputMedia(ctx, ws, ids)
	if err != nil {
		return nil, err
	}
	for id, src := range srcs {
		if src.OperationCode != smartcutdomain.OpTextToSpeech {
			continue
		}
		var parts []smartcutdomain.ScriptPart
		if len(src.Inputs) > 0 && json.Unmarshal(src.Inputs, &parts) != nil {
			parts = nil
		}
		if text := smartcutdomain.SpokenScript(parts, src.Prompt); text != "" {
			out[id] = text
		}
	}
	return out, nil
}

func (a smartcutMaterial) SaveCaptions(ctx context.Context, ws int64, model string, caps map[int64]string) error {
	var firstErr error
	for id, text := range caps {
		if err := a.media.SaveCaption(ctx, ws, id, model, text); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ---------------------------------------------------------------- 缩略图拼图（SC-PARAM-24）

// 拼图口径：每张 3 行 × 3 格，一格 160×160（不足的按比例留边），每行一段素材的首 / 中 / 尾三帧；图片只占一格。
const (
	mosaicRows = 3
	mosaicCols = 3
	mosaicCell = 160
	// mosaicQuality JPEG 质量：拼图只给模型认画面，80 足够、一张几十 KB。
	mosaicQuality = 80
)

// Mosaics 上传素材的缩略图拼图：视频取预览分析的缩略图（每秒一帧），图片取 160 宽的缩略图。
// 还没分析出缩略图的视频不进拼图（规划照样能按时长与标题剪）。
func (a smartcutMaterial) Mosaics(ctx context.Context, ws int64, items []smartcutapp.MosaicItem, maxSheets int) (
	[]smartcutapp.Mosaic, error) {
	type row struct {
		no     string
		frames []image.Image
	}
	var rows []row
	for _, it := range items {
		if len(rows) >= maxSheets*mosaicRows {
			break
		}
		var frames []image.Image
		var err error
		switch it.Kind {
		case "video":
			frames, err = a.videoFrames(ctx, ws, it.MediaID)
		case "image":
			var img image.Image
			img, err = a.decodeMedia(ctx, ws, it.MediaID, 160)
			if img != nil {
				frames = []image.Image{img}
			}
		}
		if err != nil || len(frames) == 0 {
			continue
		}
		rows = append(rows, row{no: it.No, frames: frames})
	}
	var out []smartcutapp.Mosaic
	for start := 0; start < len(rows); start += mosaicRows {
		end := min(start+mosaicRows, len(rows))
		var cells [][]image.Image
		var nos []string
		for _, r := range rows[start:end] {
			cells = append(cells, r.frames)
			nos = append(nos, r.no)
		}
		data, err := ComposeMosaic(cells)
		if err != nil {
			return nil, err
		}
		out = append(out, smartcutapp.Mosaic{DataURI: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data),
			Rows: nos})
	}
	return out, nil
}

// videoFrames 从缩略图拼图里取首 / 中 / 尾三帧：第 i 帧在第 i/100 张、第 (i%100)/Cols 行、第 (i%100)%Cols 列。
func (a smartcutMaterial) videoFrames(ctx context.Context, ws, mediaID int64) ([]image.Image, error) {
	pa, err := a.media.PreviewAssets(ctx, ws, mediaID)
	if err != nil {
		return nil, err
	}
	fs := pa.Filmstrip
	if fs == nil || fs.Count <= 0 || fs.Cols <= 0 || fs.FrameW <= 0 || fs.FrameH <= 0 {
		return nil, nil
	}
	sheets := map[int]image.Image{}
	var out []image.Image
	for _, idx := range []int{0, fs.Count / 2, fs.Count - 1} {
		s := idx / 100
		if s >= len(fs.Sheets) {
			continue
		}
		sheet, ok := sheets[s]
		if !ok {
			sheet, err = a.decodeMedia(ctx, ws, fs.Sheets[s], 0)
			if err != nil {
				return nil, err
			}
			sheets[s] = sheet
		}
		k := idx % 100
		x, y := (k%fs.Cols)*fs.FrameW, (k/fs.Cols)*fs.FrameH
		rect := image.Rect(x, y, x+fs.FrameW, y+fs.FrameH).Add(sheet.Bounds().Min)
		if !rect.In(sheet.Bounds()) {
			continue
		}
		if sub, ok := sheet.(interface {
			SubImage(r image.Rectangle) image.Image
		}); ok {
			out = append(out, sub.SubImage(rect))
		}
	}
	return out, nil
}

func (a smartcutMaterial) decodeMedia(ctx context.Context, ws, mediaID int64, width int) (image.Image, error) {
	rc, _, err := a.media.Download(ctx, ws, mediaID, width)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	img, _, err := image.Decode(io.LimitReader(rc, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("缩略图解不开：%w", err)
	}
	return img, nil
}

// ComposeMosaic 把每行至多三帧拼成一张 JPEG（3×3 格、一格 160×160，等比缩放居中、深灰留边）。
func ComposeMosaic(rows [][]image.Image) ([]byte, error) {
	w, h := mosaicCols*mosaicCell, max(len(rows), 1)*mosaicCell
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.RGBA{R: 0x20, G: 0x20, B: 0x20, A: 0xff}), image.Point{}, xdraw.Src)
	for r, frames := range rows {
		for c, f := range frames {
			if c >= mosaicCols || f == nil {
				break
			}
			b := f.Bounds()
			if b.Dx() <= 0 || b.Dy() <= 0 {
				continue
			}
			scale := min(float64(mosaicCell)/float64(b.Dx()), float64(mosaicCell)/float64(b.Dy()))
			dw, dh := max(int(float64(b.Dx())*scale), 1), max(int(float64(b.Dy())*scale), 1)
			x0, y0 := c*mosaicCell+(mosaicCell-dw)/2, r*mosaicCell+(mosaicCell-dh)/2
			xdraw.ApproxBiLinear.Scale(canvas, image.Rect(x0, y0, x0+dw, y0+dh), f, b, xdraw.Over, nil)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: mosaicQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------- 分镜成片的装配（SCT-28）

// SmartcutBoard 把脚本域接成分镜成片的装配：按素材条的顺序逐个脚本 / 视频组取镜头，镜头的取用区间以产出的实际时长为准
// （脚本里的计划时长是「打算拍多久」，CLAUDE.md §5.10）。缺素材、失败、时长未知的逐条报出来，不静默跳过（NEW-09）。
type SmartcutBoard struct {
	Script smartcutBoardScripts
	Media  *mediaapp.Service
}

// smartcutBoardScripts 是脚本域的那一个方法（集成测试可以换成真服务）。
type smartcutBoardScripts interface {
	BoardShots(ctx context.Context, workspaceID, scriptID, groupID int64) ([]scriptBoardShot, error)
}

func (a SmartcutBoard) Assemble(ctx context.Context, ws int64, groups []smartcutapp.ShotGroup) (
	[]smartcutdomain.BoardShot, []smartcutapp.BoardGap, error) {
	var shots []smartcutdomain.BoardShot
	var gaps []smartcutapp.BoardGap
	for _, g := range groups {
		rows, err := a.Script.BoardShots(ctx, ws, g.ScriptID, g.GroupID)
		if err != nil {
			return nil, nil, err
		}
		var ids []int64
		for _, r := range rows {
			if r.MediaID > 0 {
				ids = append(ids, r.MediaID)
			}
		}
		facts := map[int64]mediaapp.InputFact{}
		if len(ids) > 0 {
			if facts, err = a.Media.InputFacts(ctx, ws, ids); err != nil {
				return nil, nil, err
			}
		}
		if len(rows) == 0 {
			gaps = append(gaps, smartcutapp.BoardGap{Source: g.Title, Reason: "脚本里还没有镜头"})
		}
		for _, r := range rows {
			gap := smartcutapp.BoardGap{Source: g.Title, ShotNo: r.ShotNo, Reason: r.Reason}
			if r.MediaID == 0 {
				gaps = append(gaps, gap)
				continue
			}
			f := facts[r.MediaID]
			switch {
			case f.Invalid != "":
				gap.Reason = f.Invalid
				gaps = append(gaps, gap)
				continue
			case f.DurationMs < timelinedomain.MinClipMs:
				// 时长未知不猜一个数（CLAUDE.md §5.10）：报缺口，用户补探测或换一段。
				gap.Reason = "视频时长未知"
				gaps = append(gaps, gap)
				continue
			}
			shot := smartcutdomain.BoardShot{ShotNo: r.ShotNo, Source: g.Title, SourceNodeID: g.SourceNodeID,
				MediaID: r.MediaID, DurationMs: f.DurationMs, SourceInMs: 0, SourceOutMs: f.DurationMs,
				Dialogue: r.Dialogue, Description: r.Description}
			if f.HasAudio != nil {
				shot.HasAudio = *f.HasAudio
			}
			shots = append(shots, shot)
		}
	}
	return shots, gaps, nil
}
