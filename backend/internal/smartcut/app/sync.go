package app

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 输入同步与素材条（docs/agent/smartcut SCT-25，SCD-22 / SCD-23）。
//
// 服务端按**已保存的画布**解析上游（不信客户端报的输入，SCK-16）：生成节点回查 generation 表
// （同项目、真实状态、全部产物），按媒体声明的回查媒体表（本工作区、拒平台素材）。

// 同步方式（契约 syncSmartcutInputs.mode）。
const (
	SyncAutofill       = "autofill"
	SyncListOnly       = "listOnly"
	SyncReplaceChanged = "replaceChanged"
)

// CanvasReader 读已保存的画布，给出节点的上游（wiring：canvas/app + canvas/domain.InboundSources）。
type CanvasReader interface {
	Sources(ctx context.Context, workspaceID, projectID int64, nodeID string) ([]domain.Source, error)
}

// GenerationInfo 回查到的一次生成。
type GenerationInfo struct {
	Status       string
	Operation    string
	ErrorMessage string
	// OutputMediaIDs 全部产物的媒体（按 idx）。受理侧的 OutputLookup 只给第一个，多产物要全部。
	OutputMediaIDs []int64
}

// MediaInfo 回查到的一份媒体。Invalid 非空表示不能用（别的工作区、平台素材、已删除）。
type MediaInfo struct {
	Kind       string
	DurationMs int
	PosterID   int64
	HasAudio   *bool
	Invalid    string
}

// InputFacts 回查上游的事实（wiring：generation/repo + media/app）。
type InputFacts interface {
	// Generations 本工作区本项目的生成；查不到的不在结果里。
	Generations(ctx context.Context, workspaceID, projectID int64, ids []int64) (map[int64]GenerationInfo, error)
	// Media 每个请求的 ID 都有一条：能用的给事实，不能用的给原因。
	Media(ctx context.Context, workspaceID int64, ids []int64) (map[int64]MediaInfo, error)
}

// AnalysisQueue 给素材排预览分析（wiring：media/app.EnqueueAnalyses）。
type AnalysisQueue interface {
	EnqueueAnalyses(ctx context.Context, workspaceID int64, mediaIDs []int64, kinds []string) (int, error)
}

// SyncState 同步要的时间线状态。
type SyncState struct {
	Version    int
	IsArranged bool
	Clips      []SyncClip
}

// SyncClip 时间线里的一段（判断「用到了」与「来源已断开」）。
type SyncClip struct {
	ID           int64
	SourceNodeID string
	MediaID      int64
}

// SyncAppend 一次铺轨（wiring：timeline/app.AppendSynced）。
type SyncAppend struct {
	BaseVersion int
	Items       []SyncAppendItem
	Fingerprint string
	// Planned 自动铺轨（还没剪过时）把指纹同时记成「剪辑依据的素材」；只更新素材条时不记。
	Planned bool
}

// SyncAppendItem 一条要铺上轨道的素材。
type SyncAppendItem struct {
	MediaID      int64
	Kind         string
	DurationMs   int
	AudioRole    string
	SourceNodeID string
}

// SyncTimelines 时间线的读与铺轨（wiring：timeline/app）。
type SyncTimelines interface {
	SyncState(ctx context.Context, workspaceID, timelineID int64) (SyncState, error)
	// AppendSynced 返回新的状态与每一条的片段 ID（没铺上的为 0）。版本对不上时返回 40902。
	AppendSynced(ctx context.Context, workspaceID, timelineID int64, in SyncAppend) (SyncState, []int64, error)
}

// InputPorts 输入同步要的端口。
type InputPorts struct {
	Canvas    CanvasReader
	Facts     InputFacts
	Timelines SyncTimelines
	Analysis  AnalysisQueue
	Log       *slog.Logger
}

// SetInputPorts 装上输入同步的端口。
func (s *Service) SetInputPorts(p InputPorts) { s.inputs = p }

// InputsResult 素材条（GET .../inputs）。
type InputsResult struct {
	Items       []domain.Input
	Omitted     int
	Fingerprint string
}

// SyncResult 同步结果（契约 SmartcutSyncResult）。
type SyncResult struct {
	Inputs              []domain.Input
	Omitted             int
	AppendedClipIDs     []int64
	DisconnectedClipIDs []int64
	// ReplacedClipIDs / Shortened 原位替换（replaceChanged）换了哪些片段、哪些被截短（SCT-30）。
	ReplacedClipIDs []int64
	Shortened       []Shortened
	UnusedCount     int
	Fingerprint     string
	TimelineVersion int
}

// analysisKinds 素材条里的视频 / 音频要的预览分析（种类对不上媒体的由媒体域过滤）。
var analysisKinds = []string{"preview_proxy", "filmstrip", "peaks", "beats"}

// Inputs 只读素材条（GET /projects/{pid}/smartcuts/{tid}/inputs）。回收站里的项目也能读。
func (s *Service) Inputs(ctx context.Context, workspaceID, projectID, timelineID int64) (InputsResult, error) {
	if err := s.projectReadable(ctx, workspaceID, projectID); err != nil {
		return InputsResult{}, err
	}
	nodeID, err := s.boundNode(ctx, workspaceID, projectID, timelineID)
	if err != nil {
		return InputsResult{}, err
	}
	sources, err := s.inputs.Canvas.Sources(ctx, workspaceID, projectID, nodeID)
	if err != nil {
		return InputsResult{}, canvasError(err)
	}
	lookup, err := s.lookup(ctx, workspaceID, projectID, sources)
	if err != nil {
		return InputsResult{}, err
	}
	st, err := s.inputs.Timelines.SyncState(ctx, workspaceID, timelineID)
	if err != nil {
		return InputsResult{}, err
	}
	items, omitted := domain.ResolveInputs(sources, lookup, usedClips(st.Clips))
	return InputsResult{Items: items, Omitted: omitted, Fingerprint: domain.InputFingerprint(items)}, nil
}

// Sync 重新解析输入并按 mode 处理（POST /projects/{pid}/smartcuts/{tid}/sync），幂等。
//
//   - autofill：还没剪过时把就绪、没用到的素材按编号接到轨道末尾（连线即入轨，SC-F-13）；剪过之后只更新素材条；
//   - listOnly：只更新素材条（剪辑台开着时用，SC-F-14）。
//
// 两种都记下这次看到的素材指纹；都给缺预览素材的视频 / 音频排分析。片段的来源已断开时只报不删（SC-F-15）。
func (s *Service) Sync(ctx context.Context, workspaceID, projectID, timelineID int64, mode string) (SyncResult, error) {
	switch mode {
	case SyncAutofill, SyncListOnly:
	case SyncReplaceChanged:
		// 原位替换要 baseVersion 与操作者（它写时间线、存快照）：走 ReplaceChanged，不在这里静默当成 listOnly。
		return SyncResult{}, httpx.Errf(errcode.InvalidArgument, "原位替换请带上 baseVersion").
			WithDetails(map[string]any{"path": "baseVersion"})
	default:
		return SyncResult{}, httpx.Errf(errcode.InvalidArgument, "mode 只能是 autofill / listOnly / replaceChanged").
			WithDetails(map[string]any{"path": "mode"})
	}
	if err := s.projectWritable(ctx, workspaceID, projectID, "同步智能剪辑的素材"); err != nil {
		return SyncResult{}, err
	}
	nodeID, err := s.boundNode(ctx, workspaceID, projectID, timelineID)
	if err != nil {
		return SyncResult{}, err
	}
	sources, err := s.inputs.Canvas.Sources(ctx, workspaceID, projectID, nodeID)
	if err != nil {
		return SyncResult{}, canvasError(err)
	}
	lookup, err := s.lookup(ctx, workspaceID, projectID, sources)
	if err != nil {
		return SyncResult{}, err
	}

	// 并发写（用户刚好在别处保存了一次）撞上乐观锁时重来一次：重读之后多半已经「剪过」，就只记指纹。
	var res SyncResult
	for attempt := 0; ; attempt++ {
		res, err = s.syncOnce(ctx, workspaceID, timelineID, mode, sources, lookup)
		if err == nil || attempt > 0 || !isVersionConflict(err) {
			break
		}
	}
	if err != nil {
		return SyncResult{}, err
	}
	s.enqueueAnalyses(ctx, workspaceID, res.Inputs)
	return res, nil
}

func (s *Service) syncOnce(ctx context.Context, workspaceID, timelineID int64, mode string, sources []domain.Source,
	lookup domain.Lookup) (SyncResult, error) {
	st, err := s.inputs.Timelines.SyncState(ctx, workspaceID, timelineID)
	if err != nil {
		return SyncResult{}, err
	}
	items, omitted := domain.ResolveInputs(sources, lookup, usedClips(st.Clips))
	fp := domain.InputFingerprint(items)
	in := SyncAppend{BaseVersion: st.Version, Fingerprint: fp, Planned: mode == SyncAutofill}
	var picked []int // 要铺的素材在 items 里的下标
	if mode == SyncAutofill && !st.IsArranged {
		for i, it := range items {
			if it.Status != domain.InputReady || it.Used || !domain.Placeable(it.Kind) {
				continue
			}
			mediaID, _ := strconv.ParseInt(it.MediaID, 10, 64)
			if mediaID <= 0 {
				continue
			}
			dur := 0
			if it.DurationMs != nil {
				dur = *it.DurationMs
			}
			in.Items = append(in.Items, SyncAppendItem{MediaID: mediaID, Kind: it.Kind, DurationMs: dur,
				AudioRole: it.AudioRole, SourceNodeID: it.SourceNodeID})
			picked = append(picked, i)
		}
	}
	next, ids, err := s.inputs.Timelines.AppendSynced(ctx, workspaceID, timelineID, in)
	if err != nil {
		return SyncResult{}, err
	}
	res := SyncResult{Omitted: omitted, Fingerprint: fp, TimelineVersion: next.Version, AppendedClipIDs: []int64{}}
	for k, id := range ids {
		if id != 0 && k < len(picked) {
			items[picked[k]].Used = true
			res.AppendedClipIDs = append(res.AppendedClipIDs, id)
		}
	}
	res.Inputs = items
	res.UnusedCount = domain.UnusedCount(items)
	refs := make([]domain.ClipRef, 0, len(next.Clips))
	for _, c := range next.Clips {
		refs = append(refs, domain.ClipRef{ID: strconv.FormatInt(c.ID, 10), SourceNodeID: c.SourceNodeID})
	}
	for _, id := range domain.DisconnectedClipIDs(refs, sources) {
		n, _ := strconv.ParseInt(id, 10, 64)
		res.DisconnectedClipIDs = append(res.DisconnectedClipIDs, n)
	}
	if res.DisconnectedClipIDs == nil {
		res.DisconnectedClipIDs = []int64{}
	}
	return res, nil
}

// boundNode 时间线绑定的节点；不在本项目或没绑节点时 404。
func (s *Service) boundNode(ctx context.Context, workspaceID, projectID, timelineID int64) (string, error) {
	node, ok, err := s.timelines.TimelineNode(ctx, workspaceID, projectID, timelineID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", httpx.Errf(errcode.NotFound, "智能剪辑不存在")
	}
	return node, nil
}

// lookup 回查上游的事实。先用「记录」跑一遍解析，拿到要查的生成与媒体（取哪些 ID 的规则只在 domain 里一份），
// 再一次批量查齐 —— 不按条逐个查库。
func (s *Service) lookup(ctx context.Context, workspaceID, projectID int64, sources []domain.Source) (domain.Lookup, error) {
	rec := &recorder{gens: map[string]bool{}, media: map[string]bool{}}
	domain.ResolveInputs(sources, rec, nil)

	genIDs := parseIDs(rec.gens)
	gens, err := s.inputs.Facts.Generations(ctx, workspaceID, projectID, genIDs)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	mediaSet := map[int64]bool{}
	for _, id := range parseIDs(rec.media) {
		mediaSet[id] = true
	}
	for _, g := range gens {
		for _, m := range g.OutputMediaIDs {
			mediaSet[m] = true
		}
	}
	mediaIDs := make([]int64, 0, len(mediaSet))
	for id := range mediaSet {
		mediaIDs = append(mediaIDs, id)
	}
	media, err := s.inputs.Facts.Media(ctx, workspaceID, mediaIDs)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return &factLookup{gens: gens, media: media, asked: rec.gens}, nil
}

type recorder struct{ gens, media map[string]bool }

func (r *recorder) Generation(id string) (domain.GenerationFact, bool) {
	r.gens[id] = true
	return domain.GenerationFact{}, false
}

func (r *recorder) Media(id string) (domain.MediaFact, bool) {
	r.media[id] = true
	return domain.MediaFact{}, false
}

// factLookup 把回查结果交给 domain 的解析。
type factLookup struct {
	gens  map[int64]GenerationInfo
	media map[int64]MediaInfo
	asked map[string]bool
}

// maxReasonChars 失败原因在素材卡片上最多显示的字数（上游报错可能是一大段原文）。
const maxReasonChars = 200

func (l *factLookup) Generation(id string) (domain.GenerationFact, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return domain.GenerationFact{Status: domain.InputInvalid, Reason: "生成 ID 不合法"}, true
	}
	g, ok := l.gens[n]
	if !ok {
		// 查不到 = 不是本工作区本项目的生成（或不存在）：服务端不信画布上挂着的 ID（SC-AC-06）。
		return domain.GenerationFact{Status: domain.InputInvalid, Reason: "这个生成不属于本项目"}, true
	}
	fact := domain.GenerationFact{Status: g.Status, Operation: g.Operation, OutputsKnown: true,
		Outputs: []domain.OutputFact{}}
	if g.Status == "failed" || g.Status == "cancelled" {
		fact.Reason = truncate(g.ErrorMessage, maxReasonChars)
	}
	for _, m := range g.OutputMediaIDs {
		info := l.media[m]
		if info.Invalid != "" {
			continue // 产物已被删除：不进素材条（全被删了就是「完成但没有产物」）
		}
		o := domain.OutputFact{MediaID: strconv.FormatInt(m, 10), HasAudio: info.HasAudio}
		if info.DurationMs > 0 {
			d := info.DurationMs
			o.DurationMs = &d
		}
		if info.PosterID != 0 {
			o.PosterMediaID = strconv.FormatInt(info.PosterID, 10)
		}
		fact.Outputs = append(fact.Outputs, o)
	}
	return fact, true
}

func (l *factLookup) Media(id string) (domain.MediaFact, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return domain.MediaFact{Status: domain.InputInvalid, Reason: "素材 ID 不合法"}, true
	}
	info, ok := l.media[n]
	if !ok {
		return domain.MediaFact{}, false
	}
	if info.Invalid != "" {
		return domain.MediaFact{Status: domain.InputInvalid, Reason: info.Invalid}, true
	}
	f := domain.MediaFact{Status: domain.InputReady, HasAudio: info.HasAudio}
	if info.DurationMs > 0 {
		d := info.DurationMs
		f.DurationMs = &d
	}
	if info.PosterID != 0 {
		f.PosterMediaID = strconv.FormatInt(info.PosterID, 10)
	}
	return f, true
}

// enqueueAnalyses 给素材条里就绪的视频 / 音频排预览分析。⚠️ 失败只告警：预览是加速，
// 为它让一次同步失败的话，素材条与铺轨都白做了。
func (s *Service) enqueueAnalyses(ctx context.Context, workspaceID int64, items []domain.Input) {
	if s.inputs.Analysis == nil {
		return
	}
	var ids []int64
	for _, it := range items {
		if it.Status != domain.InputReady || (it.Kind != domain.InputVideo && it.Kind != domain.InputAudio) {
			continue
		}
		if id, err := strconv.ParseInt(it.MediaID, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	if _, err := s.inputs.Analysis.EnqueueAnalyses(ctx, workspaceID, ids, analysisKinds); err != nil && s.inputs.Log != nil {
		s.inputs.Log.Warn("给素材排预览分析失败", "error", err)
	}
}

func usedClips(clips []SyncClip) []domain.UsedClip {
	out := make([]domain.UsedClip, 0, len(clips))
	for _, c := range clips {
		out = append(out, domain.UsedClip{SourceNodeID: c.SourceNodeID, MediaID: strconv.FormatInt(c.MediaID, 10)})
	}
	return out
}

func parseIDs(set map[string]bool) []int64 {
	out := make([]int64, 0, len(set))
	for s := range set {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// ErrCanvasUnreadable 画布数据解析不了（wiring 用它包装）。
var ErrCanvasUnreadable = errors.New("画布数据解析失败")

func canvasError(err error) error {
	if errors.Is(err, ErrCanvasUnreadable) {
		// 画布坏了不能当成「没有输入」：那会把所有片段都标成来源已断开（SCT-25）。
		return httpx.Errf(errcode.Internal, "画布数据解析失败，素材条暂时取不到")
	}
	return err
}

func isVersionConflict(err error) bool {
	var be *httpx.BizError
	return errors.As(err, &be) && be.Code == errcode.VersionConflict
}
