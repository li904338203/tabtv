package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 语音识别（docs/agent/smartcut SCT-33，SC-F-120~124，SCD-32）。
//
// 按**源素材**识别，按模型定价（按秒）：结果缓存在源素材的 `media_analysis(kind=asr)`，同一份素材同一组参数
// 只识别一次、只收一次费（换一条时间线再点也不再收）；并发请求靠分析行的唯一索引只建一行、只受理一次。
// 识别的输入是抽好的 16k 单声道音轨（`speech_audio`，分析扫描器产出）：还没抽好的先排队，抽好后由 API 的
// 续接扫描（ResumePendingTranscribes）接着受理。没有音轨的素材不发起、不收费。
// 任务挂在智能剪辑名下：`Origin=smartcut`、不挂画布节点、不登记作品、不发站内通知；完成后由 BindSmartcut 回写。

// TranscribePorts 识别要的端口（wiring 适配）。
type TranscribePorts struct {
	Catalog SpeechCatalog
	Gen     SpeechAcceptor
	Media   SpeechMedia
	// Timelines 读时间线；Save 是识别字幕的写入（整份保存的同一条写路径：校验、快照、乐观锁、事件，SCT-36）。
	Timelines interface {
		Content(ctx context.Context, workspaceID, timelineID int64) (timelinedomain.Content, error)
		Save(ctx context.Context, in SavePlanInput) (SavedPlan, error)
	}
	Events PlanEvents
	Log    *slog.Logger
}

// SetTranscribePorts 装上识别的端口。
func (s *Service) SetTranscribePorts(p TranscribePorts) {
	if p.Log == nil {
		p.Log = slog.Default()
	}
	s.transcribe = p
}

// SpeechCatalog 目录域：可选的识别模型与报价（与受理同一个报价函数）。
type SpeechCatalog interface {
	SpeechModels(ctx context.Context, memberLevelID int64) ([]ModelChoice, error)
	// QuoteSpeech 报一次识别的价（按秒）；不可用时第二个返回值说明原因。
	QuoteSpeech(ctx context.Context, modelCode string, memberLevelID int64, durationSec int) (SpeechQuote, *Unavailable, error)
}

// SpeechQuote 一次识别的报价。
type SpeechQuote struct {
	Credits   int
	UnitPrice int
	Unit      string
}

// SpeechAcceptor 生成域：受理一次识别（Origin=smartcut、speech2text）。
type SpeechAcceptor interface {
	AcceptSpeech(ctx context.Context, in SpeechAccept) (SpeechAccepted, error)
}

// SpeechAccept 一次识别受理。
type SpeechAccept struct {
	WorkspaceID, PayerAccountID, UserID, ProjectID, MemberLevelID int64
	ModelCode                                                     string
	// AudioMediaID 是识别用音轨（派生媒体，与源素材同一个工作区）。
	AudioMediaID   int64
	DurationSec    int
	KeepFillers    bool
	Language       string
	IdempotencyKey string
	Target         json.RawMessage
}

// SpeechAccepted 受理结果。
type SpeechAccepted struct {
	GenerationID int64
	Status       string
	Credits      int
	CreatedAt    time.Time
	Replayed     bool
}

// SpeechMedia 媒体域：素材事实、识别用音轨、识别行（规则见 media/repo/asr.go；wiring 适配，本包不碰媒体域的仓储类型）。
type SpeechMedia interface {
	Facts(ctx context.Context, workspaceID int64, ids []int64) (map[int64]SpeechFact, error)
	SpeechAudio(ctx context.Context, workspaceID int64, ids []int64) (map[int64]SpeechAudio, error)
	EnqueueSpeechAudio(ctx context.Context, workspaceID int64, ids []int64) error
	ASRRows(ctx context.Context, workspaceID int64, ids []int64, engine, paramsKey string) (map[int64]ASRRow, error)
	InsertASR(ctx context.Context, workspaceID, mediaID int64, engine, paramsKey string, pending json.RawMessage) (ASRRow, bool, error)
	AttachASR(ctx context.Context, id, generationID int64) (bool, error)
	FailASR(ctx context.Context, id int64, msg string) (bool, error)
	SucceedASR(ctx context.Context, id int64, result json.RawMessage) (bool, error)
	FailedASRCount(ctx context.Context, mediaID int64, engine, paramsKey string) (int, error)
	ListPendingASR(ctx context.Context, limit int) ([]PendingASR, error)
	ClaimPendingASR(ctx context.Context, id int64, lease time.Duration) (bool, error)
	FinishASR(ctx context.Context, generationID, workspaceID, mediaID int64, engine, paramsKey, status string,
		result json.RawMessage, msg string) (bool, error)
}

// SpeechAudio 一份素材「识别用音轨」的当前状态。
type SpeechAudio struct {
	Status     string // queued / running / success / failed；没有为空
	DerivedID  int64
	DurationMs int
	NoAudio    bool
	Error      string
}

// ASRRow 一行识别（media_analysis kind=asr）。
type ASRRow struct {
	ID        int64
	MediaID   int64
	Engine    string
	ParamsKey string
	Status    string
	Result    json.RawMessage
	CreatedAt time.Time
}

// PendingASR 一行等着受理的识别，连同识别用音轨的状态。
type PendingASR struct {
	ASRRow
	Speech SpeechAudio
}

// SpeechFact 一份素材的类型、时长与有没有音轨（nil = 不知道）。
type SpeechFact struct {
	Kind       string
	DurationMs int
	HasAudio   *bool
}

// TranscribeRequest 一次识别请求（POST .../transcribe）。
type TranscribeRequest struct {
	WorkspaceID, PayerAccountID, ProjectID, TimelineID int64
	UserID, MemberLevelID                              int64
	ClipIDs                                            []string
	ModelCode                                          string
	KeepFillers                                        bool
	Language                                           string
	Subtitles                                          bool
	DryRun                                             bool
	IdempotencyKey                                     string
}

// TranscribeSkip 不发起的素材与原因（没有音轨、已有结果、正在识别、受理失败）。
type TranscribeSkip struct {
	MediaID int64
	Reason  string
}

// TranscribeQuote 报价（dryRun，契约 SmartcutQuote）。
type TranscribeQuote struct {
	Credits     int
	UnitPrice   int
	Unit        string
	DurationSec int
	ModelCode   string
	Items       int
	Skipped     []TranscribeSkip
	Blockers    []QuoteBlocker
}

// QuoteBlocker 报价不可用的原因。
type QuoteBlocker struct {
	Code        string
	Message     string
	UserFixable bool
}

// TranscribeResult 受理结果：已受理的任务、等音轨的、不发起的、受理失败的。
type TranscribeResult struct {
	Items   []SmartcutGenerationView
	Waiting []TranscribeSkip
	Skipped []TranscribeSkip
	Failed  []TranscribeSkip
	// SubtitleError 已有识别结果的素材当场写字幕失败的原因（识别本身照常受理；空 = 没失败或不用写）。
	SubtitleError string
}

// SmartcutGenerationView 契约 SmartcutGeneration。
type SmartcutGenerationView struct {
	GenerationID int64
	Kind         string
	Status       string
	Progress     int
	Credits      int
	MediaID      int64
	// ClipID 片段级生成作用的那一段（补镜为空）。
	ClipID    string
	CreatedAt time.Time
}

// asrPending 是识别行在等音轨时记下的受理信息（`media_analysis.result`）：续接扫描靠它替用户受理。
type asrPending struct {
	WorkspaceID    int64  `json:"workspaceId,string"`
	PayerAccountID int64  `json:"payerAccountId,string"`
	UserID         int64  `json:"userId,string"`
	ProjectID      int64  `json:"projectId,string"`
	TimelineID     int64  `json:"timelineId,string"`
	MemberLevelID  int64  `json:"memberLevelId,string"`
	ModelCode      string `json:"modelCode"`
	KeepFillers    bool   `json:"keepFillers"`
	Language       string `json:"language,omitempty"`
	Subtitles      bool   `json:"subtitles,omitempty"`
	// ClipIDs 只给这几段出字幕（识别时选的片段；空 = 整条时间线，SCT-36）。
	ClipIDs []string `json:"clipIds,omitempty"`
}

// asrTarget 是识别任务的回写目标（`generation.smartcut_target`）。ID 一律字符串（索引按 `->>'timelineId'` 取）。
type asrTarget struct {
	TimelineID int64  `json:"timelineId,string"`
	Kind       string `json:"kind"`
	MediaID    int64  `json:"mediaId,string"`
	Engine     string `json:"engine"`
	ParamsKey  string `json:"paramsKey"`
	AnalysisID int64  `json:"analysisId,string"`
	Subtitles  bool   `json:"subtitles,omitempty"`
	// ClipIDs / UserID 识别完成后写字幕用：只写这几段（空 = 整条时间线），写入记在发起识别的人名下（SCT-36）。
	ClipIDs []string `json:"clipIds,omitempty"`
	UserID  int64    `json:"userId,string,omitempty"`
}

// 跳过的原因（界面原样显示）。
const (
	skipNoAudio    = "没有音轨"
	skipCached     = "已有识别结果，不再收费"
	skipRunning    = "正在识别"
	skipImage      = "图片没有声音"
	waitingSpeech  = "正在准备识别用音轨，准备好后自动开始识别"
	pendingTimeout = 30 * time.Minute
	pendingLease   = time.Minute
)

func (s *Service) transcribeReady() error {
	p := s.transcribe
	if p.Catalog == nil || p.Gen == nil || p.Media == nil || p.Timelines == nil {
		return httpx.Errf(errcode.Internal, "语音识别链路未接入")
	}
	return nil
}

// candidate 是一份要识别的素材。
type candidate struct {
	mediaID    int64
	durationMs int
	speech     SpeechAudio
}

// plan 选材：去重、去掉图片与没有音轨的、已有结果或正在识别的，剩下的是要识别的。
func (s *Service) planTranscribe(ctx context.Context, req TranscribeRequest) ([]candidate, []TranscribeSkip, error) {
	p := s.transcribe
	content, err := p.Timelines.Content(ctx, req.WorkspaceID, req.TimelineID)
	if err != nil {
		return nil, nil, err
	}
	ids, missing := domain.TranscribeSources(content, req.ClipIDs)
	if len(missing) > 0 {
		return nil, nil, httpx.Errf(errcode.NotFound, "片段不存在："+strings.Join(missing, "、")).
			WithDetails(map[string]any{"path": "clipIds", "clipIds": missing})
	}
	facts, err := p.Media.Facts(ctx, req.WorkspaceID, ids)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	speech, err := p.Media.SpeechAudio(ctx, req.WorkspaceID, ids)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	engine, params := req.ModelCode, domain.ASRParamsKey(req.KeepFillers, req.Language)
	cached, err := p.Media.ASRRows(ctx, req.WorkspaceID, ids, engine, params)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	var out []candidate
	var skipped []TranscribeSkip
	for _, id := range ids {
		f, ok := facts[id]
		switch {
		case !ok:
			// 素材已删除或不在本工作区：时间线上会标缺素材，这里不识别。
			continue
		case f.Kind == "image":
			skipped = append(skipped, TranscribeSkip{id, skipImage})
			continue
		case f.HasAudio != nil && !*f.HasAudio:
			skipped = append(skipped, TranscribeSkip{id, skipNoAudio})
			continue
		}
		sp := speech[id]
		if sp.Status == "success" && sp.NoAudio {
			skipped = append(skipped, TranscribeSkip{id, skipNoAudio})
			continue
		}
		if row, ok := cached[id]; ok {
			reason := skipRunning
			if row.Status == "success" {
				reason = skipCached
			}
			skipped = append(skipped, TranscribeSkip{id, reason})
			continue
		}
		dur := f.DurationMs
		if sp.Status == "success" {
			dur = domain.SpeechBillMs(f.DurationMs, sp.DurationMs)
		}
		out = append(out, candidate{mediaID: id, durationMs: dur, speech: sp})
	}
	return out, skipped, nil
}

// QuoteTranscribe 报价（dryRun）：逐份素材按「向上取整到秒」报，再相加 —— 与受理时逐份扣的数目一致。
func (s *Service) QuoteTranscribe(ctx context.Context, req TranscribeRequest) (TranscribeQuote, error) {
	if err := s.transcribeReady(); err != nil {
		return TranscribeQuote{}, err
	}
	if err := validateTranscribe(req); err != nil {
		return TranscribeQuote{}, err
	}
	if err := s.projectReadable(ctx, req.WorkspaceID, req.ProjectID); err != nil {
		return TranscribeQuote{}, err
	}
	if _, err := s.boundNode(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID); err != nil {
		return TranscribeQuote{}, err
	}
	list, skipped, err := s.planTranscribe(ctx, req)
	if err != nil {
		return TranscribeQuote{}, err
	}
	out := TranscribeQuote{ModelCode: req.ModelCode, Items: len(list), Skipped: nonNilSkips(skipped),
		Blockers: []QuoteBlocker{}}
	if len(list) == 0 {
		// 没有要识别的：报 0，不去问目录（模型此刻不可用也不影响「全部已识别过」这个答案）。
		return out, nil
	}
	for _, c := range list {
		sec := domain.BillableSeconds(c.durationMs)
		q, unavailable, err := s.transcribe.Catalog.QuoteSpeech(ctx, req.ModelCode, req.MemberLevelID, sec)
		if err != nil {
			return TranscribeQuote{}, httpx.Wrap(errcode.Internal, err)
		}
		if unavailable != nil {
			out.Credits, out.Items = 0, 0
			out.Blockers = []QuoteBlocker{{Code: "unavailable", Message: unavailable.Reason, UserFixable: unavailable.UserFixable}}
			return out, nil
		}
		out.Credits += q.Credits
		out.DurationSec += sec
		out.UnitPrice, out.Unit = q.UnitPrice, q.Unit
	}
	return out, nil
}

// Transcribe 受理识别。逐份素材：插一行识别（唯一索引挡并发）→ 音轨好了就受理，没好就排队等续接扫描。
//
// ⚠️ 余额不足时停下后面的（CLAUDE.md §5.7「余额不足停掉后续项而不是逐项失败」）：一份都没受理成就把那个错误原样
// 返回（前端按业务码给充值入口），受理了一部分就在 Failed 里说清后面几份没识别。
func (s *Service) Transcribe(ctx context.Context, req TranscribeRequest) (TranscribeResult, error) {
	if err := s.transcribeReady(); err != nil {
		return TranscribeResult{}, err
	}
	if err := validateTranscribe(req); err != nil {
		return TranscribeResult{}, err
	}
	if err := s.projectWritable(ctx, req.WorkspaceID, req.ProjectID, "识别语音"); err != nil {
		return TranscribeResult{}, err
	}
	if _, err := s.boundNode(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID); err != nil {
		return TranscribeResult{}, err
	}
	list, skipped, err := s.planTranscribe(ctx, req)
	if err != nil {
		return TranscribeResult{}, err
	}
	out := TranscribeResult{Items: []SmartcutGenerationView{}, Waiting: []TranscribeSkip{}, Skipped: nonNilSkips(skipped),
		Failed: []TranscribeSkip{}}
	pending := asrPending{WorkspaceID: req.WorkspaceID, PayerAccountID: req.PayerAccountID, UserID: req.UserID,
		ProjectID: req.ProjectID, TimelineID: req.TimelineID, MemberLevelID: req.MemberLevelID, ModelCode: req.ModelCode,
		KeepFillers: req.KeepFillers, Language: req.Language, Subtitles: req.Subtitles, ClipIDs: req.ClipIDs}
	raw, _ := json.Marshal(pending)
	engine, params := req.ModelCode, domain.ASRParamsKey(req.KeepFillers, req.Language)
	var stopErr error
	for _, c := range list {
		if stopErr != nil {
			out.Failed = append(out.Failed, TranscribeSkip{c.mediaID, "余额不足，没有识别"})
			continue
		}
		row, inserted, err := s.transcribe.Media.InsertASR(ctx, req.WorkspaceID, c.mediaID, engine, params, raw)
		if err != nil {
			out.Failed = append(out.Failed, TranscribeSkip{c.mediaID, "排队识别失败：" + err.Error()})
			continue
		}
		if !inserted {
			// 并发的另一个请求刚插进去（或刚识别完）：不再受理、不再扣费。
			reason := skipRunning
			if row.Status == "success" {
				reason = skipCached
			}
			out.Skipped = append(out.Skipped, TranscribeSkip{c.mediaID, reason})
			continue
		}
		if c.speech.Status != "success" {
			// 音轨还没好（或上一次抽失败了）：排队，抽好后续接扫描接着受理。
			if c.speech.Status == "" || c.speech.Status == "failed" {
				if err := s.transcribe.Media.EnqueueSpeechAudio(ctx, req.WorkspaceID, []int64{c.mediaID}); err != nil {
					s.transcribe.Log.WarnContext(ctx, "排识别用音轨失败，等续接扫描重试", "mediaId", c.mediaID, "error", err)
				}
			}
			out.Waiting = append(out.Waiting, TranscribeSkip{c.mediaID, waitingSpeech})
			continue
		}
		gen, err := s.acceptASR(ctx, row, pending, c.speech)
		if err != nil {
			msg := errMessage(err)
			_, _ = s.transcribe.Media.FailASR(ctx, row.ID, msg)
			if errCode(err) == errcode.InsufficientCredits {
				if len(out.Items) == 0 && len(out.Waiting) == 0 {
					return TranscribeResult{}, err
				}
				stopErr = err
			}
			out.Failed = append(out.Failed, TranscribeSkip{c.mediaID, msg})
			continue
		}
		out.Items = append(out.Items, SmartcutGenerationView{GenerationID: gen.GenerationID, Kind: "asr",
			Status: gen.Status, Credits: gen.Credits, MediaID: c.mediaID, CreatedAt: gen.CreatedAt})
	}
	if req.Subtitles {
		// 已有识别结果的素材不会再有任务完成来触发写字幕：现在就写（其余的在各自识别完成时由回写写入）。
		var cached []int64
		for _, sk := range out.Skipped {
			if sk.Reason == skipCached {
				cached = append(cached, sk.MediaID)
			}
		}
		if err := s.writeASRSubtitles(ctx, asrSubtitleWrite{workspaceID: req.WorkspaceID, projectID: req.ProjectID,
			timelineID: req.TimelineID, userID: req.UserID, clipIDs: req.ClipIDs, mediaIDs: cached,
			engine: engine, paramsKey: params}); err != nil {
			s.transcribe.Log.WarnContext(ctx, "已有识别结果的素材写字幕失败", "timelineId", req.TimelineID, "error", err)
			out.SubtitleError = errMessage(err)
		}
	}
	return out, nil
}

// acceptASR 替一行识别受理任务并挂上（请求时与续接扫描共用）。
func (s *Service) acceptASR(ctx context.Context, row ASRRow, p asrPending, speech SpeechAudio) (SpeechAccepted, error) {
	failed, err := s.transcribe.Media.FailedASRCount(ctx, row.MediaID, row.Engine, row.ParamsKey)
	if err != nil {
		return SpeechAccepted{}, httpx.Wrap(errcode.Internal, err)
	}
	// 计费时长与报价同一个口径（domain.SpeechBillMs）：素材时长与音轨实测时长取小。素材查不到时只按音轨算。
	billMs := speech.DurationMs
	if facts, err := s.transcribe.Media.Facts(ctx, p.WorkspaceID, []int64{row.MediaID}); err == nil {
		billMs = domain.SpeechBillMs(facts[row.MediaID].DurationMs, speech.DurationMs)
	}
	target, _ := json.Marshal(asrTarget{TimelineID: p.TimelineID, Kind: "asr", MediaID: row.MediaID,
		Engine: row.Engine, ParamsKey: row.ParamsKey, AnalysisID: row.ID, Subtitles: p.Subtitles,
		ClipIDs: p.ClipIDs, UserID: p.UserID})
	gen, err := s.transcribe.Gen.AcceptSpeech(ctx, SpeechAccept{
		WorkspaceID: p.WorkspaceID, PayerAccountID: p.PayerAccountID, UserID: p.UserID, ProjectID: p.ProjectID,
		MemberLevelID: p.MemberLevelID, ModelCode: p.ModelCode, AudioMediaID: speech.DerivedID,
		// 时长由服务端给：素材与抽好的音轨实测时长取小（向上取整到秒），不收客户端报的数。
		DurationSec: domain.BillableSeconds(billMs), KeepFillers: p.KeepFillers, Language: p.Language,
		IdempotencyKey: domain.ASRIdempotencyKey(row.MediaID, row.Engine, row.ParamsKey, failed+1),
		Target:         target,
	})
	if err != nil {
		return SpeechAccepted{}, err
	}
	if _, err := s.transcribe.Media.AttachASR(ctx, row.ID, gen.GenerationID); err != nil {
		s.transcribe.Log.WarnContext(ctx, "识别任务已受理但挂到分析行失败", "analysisId", row.ID,
			"generationId", gen.GenerationID, "error", err)
	}
	return gen, nil
}

// ResumePendingTranscribes 续接扫描（API 进程里定时跑）：音轨抽好了就替用户受理，没有音轨就记「没有语音」，
// 抽失败或等太久就判失败。返回处理了几行。多个 API 实例同时扫也安全（逐行领取 + 受理幂等）。
func (s *Service) ResumePendingTranscribes(ctx context.Context) (int, error) {
	if s.transcribeReady() != nil {
		return 0, nil
	}
	list, err := s.transcribe.Media.ListPendingASR(ctx, 20)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, row := range list {
		if s.resumeOne(ctx, row) {
			done++
		}
	}
	return done, nil
}

func (s *Service) resumeOne(ctx context.Context, row PendingASR) bool {
	m := s.transcribe.Media
	var p asrPending
	if err := json.Unmarshal(row.Result, &p); err != nil || p.WorkspaceID == 0 {
		_, _ = m.FailASR(ctx, row.ID, "识别请求信息缺失，请重新识别")
		return true
	}
	switch {
	case row.Speech.Status == "success" && row.Speech.NoAudio:
		res, _ := json.Marshal(map[string]any{"text": "", "noAudio": true, "granularity": "text", "utterances": []any{}})
		if ok, _ := m.SucceedASR(ctx, row.ID, res); ok {
			s.publishAnalysis(ctx, p.WorkspaceID, p.ProjectID, p.TimelineID, row.MediaID, "success")
		}
		return true
	case row.Speech.Status == "success":
		claimed, err := m.ClaimPendingASR(ctx, row.ID, pendingLease)
		if err != nil || !claimed {
			return false
		}
		if _, err := s.acceptASR(ctx, row.ASRRow, p, row.Speech); err != nil {
			_, _ = m.FailASR(ctx, row.ID, errMessage(err))
			s.publishAnalysis(ctx, p.WorkspaceID, p.ProjectID, p.TimelineID, row.MediaID, "failed")
			return true
		}
		s.publishAnalysis(ctx, p.WorkspaceID, p.ProjectID, p.TimelineID, row.MediaID, "running")
		return true
	case row.Speech.Status == "failed":
		_, _ = m.FailASR(ctx, row.ID, "抽识别用音轨失败："+row.Speech.Error)
		s.publishAnalysis(ctx, p.WorkspaceID, p.ProjectID, p.TimelineID, row.MediaID, "failed")
		return true
	case s.now().Sub(row.CreatedAt) > pendingTimeout:
		_, _ = m.FailASR(ctx, row.ID, "识别用音轨一直没有准备好，请稍后重新识别")
		s.publishAnalysis(ctx, p.WorkspaceID, p.ProjectID, p.TimelineID, row.MediaID, "failed")
		return true
	case row.Speech.Status == "":
		// 没有排上（排队那一刻失败了）：再排一次，下一轮再看。
		_ = m.EnqueueSpeechAudio(ctx, p.WorkspaceID, []int64{row.MediaID})
	}
	return false
}

// BindInput 智能剪辑名下的一次任务到了终态（wiring 从生成域的任务与产物转过来）。
type BindInput struct {
	GenerationID, WorkspaceID, ProjectID int64
	Target                               json.RawMessage
	// Status success / failed / cancelled。
	Status string
	// Text / Meta 文本产物（识别结果）；MediaIDs 媒体产物（按产物顺序）。
	Text     string
	Meta     json.RawMessage
	MediaIDs []int64
	ErrMsg   string
}

// BindSmartcut 识别与片段级生成到终态时回写（实现 generation/app.SmartcutBinder 的智能剪辑侧）。
func (s *Service) BindSmartcut(ctx context.Context, in BindInput) error {
	var head struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(in.Target, &head); err != nil {
		return fmt.Errorf("回写目标解析失败: %w", err)
	}
	switch head.Kind {
	case "asr":
		return s.bindASR(ctx, in.GenerationID, in.WorkspaceID, in.ProjectID, in.Target, in.Status, in.Text, in.Meta, in.ErrMsg)
	case clipKindReplace, clipKindInsert, clipKindMusic, clipKindSfx:
		return s.bindClip(ctx, in)
	}
	return nil
}

func (s *Service) bindASR(ctx context.Context, generationID, workspaceID, projectID int64, raw json.RawMessage,
	status, text string, meta json.RawMessage, errMsg string) error {
	if s.transcribe.Media == nil {
		return errors.New("语音识别链路未接入")
	}
	var t asrTarget
	if err := json.Unmarshal(raw, &t); err != nil {
		return fmt.Errorf("识别的回写目标解析失败: %w", err)
	}
	var result json.RawMessage
	rowStatus := "failed"
	if status == "success" {
		rowStatus = "success"
		result = asrResult(text, meta)
	}
	changed, err := s.transcribe.Media.FinishASR(ctx, generationID, workspaceID, t.MediaID, t.Engine, t.ParamsKey,
		rowStatus, result, errMsg)
	if err != nil {
		return err
	}
	s.publishAnalysis(ctx, workspaceID, projectID, t.TimelineID, t.MediaID, rowStatus)
	// 识别时选了「生成字幕」：把这份素材的片段的字幕写进去（只在这一次真的写下了结果时：重复的回写不再写一遍）。
	// 写失败只告警 —— 识别结果已经存下，用户可以再点一次识别（命中缓存、不收费）把字幕补上。
	if rowStatus == "success" && changed && t.Subtitles {
		if err := s.writeASRSubtitles(ctx, asrSubtitleWrite{workspaceID: workspaceID, projectID: projectID,
			timelineID: t.TimelineID, userID: t.UserID, clipIDs: t.ClipIDs, mediaIDs: []int64{t.MediaID},
			engine: t.Engine, paramsKey: t.ParamsKey}); err != nil {
			s.transcribe.Log.WarnContext(ctx, "识别完成后写字幕失败", "timelineId", t.TimelineID, "mediaId", t.MediaID,
				"error", err)
		}
	}
	return nil
}

// asrResult 把识别产物（全文 + meta）拼成分析缓存里的结果（smartcut/domain.Transcript 的形状 + text）。
func asrResult(text string, meta json.RawMessage) json.RawMessage {
	obj := map[string]any{}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &obj)
	}
	if _, ok := obj["granularity"]; !ok {
		obj["granularity"] = "text"
	}
	if _, ok := obj["utterances"]; !ok {
		obj["utterances"] = []any{}
	}
	obj["text"] = text
	b, _ := json.Marshal(obj)
	return b
}

// publishAnalysis 推 smartcut.updated（analysis 段）：节点卡与剪辑台收到后刷新素材条与识别状态。失败只告警。
func (s *Service) publishAnalysis(ctx context.Context, workspaceID, projectID, timelineID, mediaID int64, status string) {
	p := s.transcribe
	if p.Events == nil || projectID == 0 || timelineID == 0 {
		return
	}
	node, ok, err := s.timelines.TimelineNode(ctx, workspaceID, projectID, timelineID)
	if err != nil || !ok {
		return
	}
	mid, kind := strconv.FormatInt(mediaID, 10), "asr"
	payload, err := json.Marshal(SmartcutUpdated{Type: SmartcutUpdatedType, OccurredAt: s.now().UTC().Format(time.RFC3339Nano),
		ProjectID: strconv.FormatInt(projectID, 10), TimelineID: strconv.FormatInt(timelineID, 10), CanvasNodeID: node,
		Analysis: &SmartcutAnalysisEvent{MediaID: mid, Kind: kind, Status: status}})
	if err == nil {
		err = p.Events.Publish(ctx, "project:"+strconv.FormatInt(projectID, 10), payload)
	}
	if err != nil {
		p.Log.WarnContext(ctx, "推送识别状态失败", "mediaId", mediaID, "error", err)
	}
}

func validateTranscribe(req TranscribeRequest) error {
	switch {
	case strings.TrimSpace(req.ModelCode) == "":
		return httpx.Errf(errcode.InvalidArgument, "请选择识别模型").WithDetails(map[string]any{"path": "modelCode"})
	case !domain.TranscribeLanguages[req.Language]:
		return httpx.Errf(errcode.InvalidArgument, "language 只能是 zh-CN / en / ja / ko").
			WithDetails(map[string]any{"path": "language"})
	case !req.DryRun && (len(req.IdempotencyKey) < 8 || len(req.IdempotencyKey) > 128):
		return httpx.Errf(errcode.MissingIdempotencyKey, "识别会扣费，请求要带 Idempotency-Key（8~128 位）")
	}
	return nil
}

func nonNilSkips(s []TranscribeSkip) []TranscribeSkip {
	if s == nil {
		return []TranscribeSkip{}
	}
	return s
}

func errCode(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

func errMessage(err error) string {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Error()
	}
	return "受理识别失败"
}

// withTranscribeQuote 给 42217 asr_missing 补上识别的出路信息（SC-UX-09「先识别语音 ✦N」）：缺识别结果的素材编号
// 与按可选的第一个识别模型报的价。口播精剪只认字级 / 词级的模型，解说成片同样按字对齐，所以只取这两种粒度的。
// 取不到（没有可用的识别模型、报价出错）时原样返回，不让补充信息把原来的错误盖掉。
func (s *Service) withTranscribeQuote(ctx context.Context, req PlanRequest, items []domain.Input, err error) error {
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.SmartcutInputUnsuitable || be.Details["reason"] != domain.UnsuitableASRMissing ||
		s.transcribe.Catalog == nil || s.plan.Material == nil {
		return err
	}
	var ids []int64
	byMedia := map[int64]domain.Input{}
	for _, it := range items {
		want := (req.Mode == domain.ModeTalk && it.Kind == domain.InputVideo) ||
			(req.Mode == domain.ModeNarr && it.Kind == domain.InputAudio && it.AudioRole == domain.RoleVoice)
		id, perr := strconv.ParseInt(it.MediaID, 10, 64)
		if !want || it.Status != "ready" || perr != nil || id <= 0 {
			continue
		}
		if _, seen := byMedia[id]; !seen {
			ids = append(ids, id)
			byMedia[id] = it
		}
	}
	need := narrNeed
	if req.Mode == domain.ModeTalk {
		need = talkNeed
	}
	have, _, terr := s.plan.Material.Transcripts(ctx, req.WorkspaceID, ids, need)
	if terr != nil {
		return err
	}
	models, merr := s.transcribe.Catalog.SpeechModels(ctx, req.MemberLevelID)
	if merr != nil {
		return err
	}
	model := ""
	for _, m := range models {
		if m.Granularity == "char" || m.Granularity == "word" {
			model = m.Code
			break
		}
	}
	missing := []string{}
	credits, seconds := 0, 0
	for _, id := range ids {
		if _, ok := have[id]; ok {
			continue
		}
		it := byMedia[id]
		missing = append(missing, it.No)
		dur := 0
		if it.DurationMs != nil {
			dur = *it.DurationMs
		}
		seconds += domain.BillableSeconds(dur)
		if model != "" {
			q, unavailable, qerr := s.transcribe.Catalog.QuoteSpeech(ctx, model, req.MemberLevelID, domain.BillableSeconds(dur))
			if qerr != nil || unavailable != nil {
				model = ""
				continue
			}
			credits += q.Credits
		}
	}
	details := map[string]any{}
	for k, v := range be.Details {
		details[k] = v
	}
	details["missing"] = missing
	if model != "" && len(missing) > 0 {
		details["quote"] = map[string]any{"credits": credits, "modelCode": model, "durationSec": seconds,
			"keepFillers": req.Mode == domain.ModeTalk}
	}
	return httpx.Errf(errcode.SmartcutInputUnsuitable, be.Message).WithDetails(details)
}
