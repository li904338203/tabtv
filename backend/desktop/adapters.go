package desktop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	contentapp "github.com/li904338203/tabtv/backend/internal/content/app"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediadomain "github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	providertransport "github.com/li904338203/tabtv/backend/internal/provider/transport"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 本文件是从来源系统 cmd/api 与 cmd/worker 搬来的端口适配器。两个进程原本各抄一份的（媒体引用检查、
// 退款、归档）在这里只有一份。

type tokenService struct{ svc *token.Service }

// newBillingService 只为 Agent 读余额与额度（本机账户的余额无所谓多少：全部价格为 0，见 TT-T-09）。
func newBillingService(pool *pgxpool.Pool, nextID func() int64, a *audit.Logger, log *slog.Logger) *billingapp.Service {
	s := billingapp.New(billingrepo.New(pool, nextID))
	s.SetAudit(a)
	s.SetLogger(log)
	return s
}

// registerMediaRefs：删除与回收前当场问各域「还在用吗」（计算式删除保护，来源系统 P14-03）。
// 来源系统要求「两个进程都注册」，单进程下只有这一处。
func registerMediaRefs(m *mediaapp.Service, pool *pgxpool.Pool, nextID func() int64) {
	add := func(kind string, fn func(context.Context, int64) ([]int64, error)) {
		m.AddReferenceChecker(mediaRefChecker{kind, fn})
	}
	add(mediaapp.UsageScriptAsset, scriptrepo.New(pool).MediaReferences)
	add(mediaapp.UsageTimelineClip, timelinerepo.New(pool, nextID).MediaReferences)
	add(mediaapp.UsageGenerationOut, genrepo.New(pool).MediaReferences)
	add(mediaapp.UsageProjectCover, canvasrepo.New(pool).MediaReferences)
	add(mediaapp.UsageAgentSession, agentrepo.New(pool).GeneralMediaReferences)
	add(mediaapp.UsageAgentMarketing, agentrepo.New(pool).MarketingMediaReferences)
	add(mediaapp.UsageMarketingProduct, marketingrepo.New(pool).ProductMediaReferences)
	add(mediaapp.UsageMarketingCharacter, marketingrepo.New(pool).CharacterMediaReferences)
	add(mediaapp.UsageDirectorScene, canvasrepo.New(pool).DirectorMediaReferences)
}

type mediaRefChecker struct {
	kind string
	fn   func(context.Context, int64) ([]int64, error)
}

func (c mediaRefChecker) Kind() string { return c.kind }
func (c mediaRefChecker) References(ctx context.Context, mediaID int64) ([]int64, error) {
	return c.fn(ctx, mediaID)
}

// providerProber 把传输层的探活接成目录域的连接测试端口。
type providerProber struct {
	svc    *catalogapp.Service
	client *providertransport.Client
}

func (p providerProber) Probe(ctx context.Context, code string) (any, error) {
	cfg, err := p.svc.ProviderForProbe(ctx, code)
	if err != nil {
		return nil, err
	}
	creds := providertransport.Credentials{Type: cfg.AuthType, Key: cfg.APIKey, HeaderName: cfg.HeaderName,
		Region: cfg.SignRegion, Service: cfg.SignService}
	header, err := creds.Headers("GET", providertransport.ProbeURL(cfg.BaseURL, cfg.TestPath), nil, nil, time.Now())
	if err != nil {
		return providertransport.ProbeResult{OK: false, ErrorKind: providertransport.ErrorPolicy,
			Message: err.Error()}, nil
	}
	return p.client.Probe(ctx, cfg.BaseURL, cfg.TestPath, header,
		providertransport.Options{
			ConnectTimeout: time.Duration(cfg.ConnectTimeoutMS) * time.Millisecond,
			RequestTimeout: time.Duration(cfg.RequestTimeoutMS) * time.Millisecond,
		}), nil
}

// workSourceGuard：内容域「我的资产」登记来源时校验归属与成功（来源系统 P13-02）。
type workSourceGuard struct {
	gen *genapp.Service
	tl  *timelineapp.Service
}

func (g workSourceGuard) Resolve(r *http.Request, workspaceID int64,
	sourceType string, sourceID int64) (string, int64, error) {
	ctx := r.Context()
	switch sourceType {
	case "generation":
		item, err := g.gen.Get(ctx, workspaceID, sourceID)
		if err != nil {
			return "", 0, err
		}
		if item.Status != "success" {
			return "", 0, httpx.Errf(errcode.InvalidStateTransition, "只有已成功的任务才能提交为作品")
		}
		outs, err := g.gen.Outputs(ctx, workspaceID, sourceID)
		if err != nil {
			return "", 0, err
		}
		for _, o := range outs {
			if o.MediaObjectID != 0 {
				return "", o.MediaObjectID, nil
			}
		}
		return "", 0, httpx.Errf(errcode.InvalidStateTransition, "这次生成没有可展示的媒体产出")
	case "render":
		job, err := g.tl.GetRender(ctx, workspaceID, sourceID)
		if err != nil {
			return "", 0, err
		}
		if job.Status != "success" || job.OutputMediaID == 0 {
			return "", 0, httpx.Errf(errcode.InvalidStateTransition, "只有已成功的渲染才能提交为作品")
		}
		return "", job.OutputMediaID, nil
	}
	return "", 0, httpx.Errf(errcode.InvalidArgument, "来源类型非法")
}

// skillModels 把模型目录接成技能配置的模型下拉端口。
type skillModels struct{ catalog *catalogapp.Service }

func (m skillModels) SkillModels(ctx context.Context) ([]agentapp.SkillModelInfo, error) {
	list, err := m.catalog.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agentapp.SkillModelInfo, 0, len(list))
	for _, x := range list {
		out = append(out, agentapp.SkillModelInfo{Code: x.Code, Name: x.Name, Domain: x.Domain,
			Status: x.Status, AgentCapable: x.IsAgentCapable})
	}
	return out, nil
}

// projectAgents 把画布仓储的会话摘要接成首页最近项目的端口。
type projectAgents struct{ repo *canvasrepo.Repo }

func (p projectAgents) ProjectAgents(ctx context.Context, workspaceID int64,
	projectIDs []int64) (map[int64]contentapp.ProjectAgent, error) {
	m, err := p.repo.AgentSummaries(ctx, workspaceID, projectIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]contentapp.ProjectAgent, len(m))
	for id, b := range m {
		out[id] = contentapp.ProjectAgent{SessionID: b.SessionID, Title: b.Title,
			State: b.State, LastActiveAt: b.LastActiveAt}
	}
	return out, nil
}

// ---------------------------------------------------------------- 执行侧适配（原 cmd/worker）

// mediaRefAdapter 把媒体域接成 Provider 取参考媒体的端口。
type mediaRefAdapter struct {
	svc   *mediaapp.Service
	store *objstore.Client
}

func (a mediaRefAdapter) SignedURL(ctx context.Context, mediaID int64) (string, string, error) {
	m, err := a.svc.Get(ctx, mediaID)
	if err != nil {
		return "", "", err
	}
	url, err := a.store.PresignGet(ctx, m.ObjectKey)
	return url, m.ContentType, err
}

func (a mediaRefAdapter) DurationMs(ctx context.Context, mediaID int64) (int, error) {
	m, err := a.svc.Get(ctx, mediaID)
	if err != nil {
		return 0, err
	}
	return m.DurationMs, nil
}

func (a mediaRefAdapter) Open(ctx context.Context, mediaID int64) (io.ReadCloser, string, error) {
	body, m, err := a.svc.Open(ctx, mediaID)
	if err != nil {
		return nil, "", err
	}
	return body, m.ContentType, nil
}

func (a mediaRefAdapter) Inline(ctx context.Context, mediaID int64) ([]byte, string, error) {
	body, m, err := a.svc.Open(ctx, mediaID)
	if err != nil {
		return nil, "", err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	return data, m.ContentType, err
}

type prefixedScratch struct {
	store  *objstore.Client
	prefix string
}

func (s prefixedScratch) PutFile(ctx context.Context, key string, body io.ReadSeeker, size int64, mime string) error {
	return s.store.PutFile(ctx, s.prefix+key, body, size, mime)
}
func (s prefixedScratch) PresignGet(ctx context.Context, key string) (string, error) {
	return s.store.PresignGet(ctx, s.prefix+key)
}
func (s prefixedScratch) Delete(ctx context.Context, key string) error {
	return s.store.Delete(ctx, s.prefix+key)
}

// archiverAdapter：执行器把产物归档进媒体库（先归档再标成功）。
type archiverAdapter struct{ svc *mediaapp.Service }

func (a archiverAdapter) Archive(ctx context.Context, in genapp.ArchiveInput) (int64, error) {
	up, err := a.svc.Upload(ctx, mediaapp.UploadInput{
		WorkspaceID: in.WorkspaceID, UserID: in.UserID, Purpose: "generation_output",
		DeclaredMIME: in.MIME, Size: in.Size, Body: in.Body,
	})
	if err != nil {
		return 0, err
	}
	return up.ID, nil
}

func (a archiverAdapter) RegisterUsage(ctx context.Context, mediaID int64, usageType string, refID int64) error {
	return a.svc.RegisterUsage(ctx, mediaID, usageType, refID)
}

// refunderAdapter：执行器失败退款（价格为 0 时账本里根本没有消费，退款是空操作）。
type refunderAdapter struct {
	repo       *billingrepo.Repo
	compensate time.Duration
}

func (a refunderAdapter) Refund(ctx context.Context, payerID int64, refType, refID, reason string) error {
	_, err := a.repo.Refund(ctx, billingrepo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID, Reason: reason,
		CompensationValidFor: a.compensate,
	})
	if errors.Is(err, billingrepo.ErrDuplicate) {
		return nil
	}
	return err
}

// renderFetcher：渲染素材落临时文件（不落内存），并探一次流信息。
type renderFetcher struct{ svc *mediaapp.Service }

func (a renderFetcher) Fetch(ctx context.Context, mediaID int64) (timelinedomain.MediaFile, func(), error) {
	body, m, err := a.svc.Open(ctx, mediaID)
	if err != nil {
		return timelinedomain.MediaFile{}, nil, err
	}
	defer body.Close()
	f, err := os.CreateTemp("", "tabtv-src-*"+mediaExt(m.ContentType))
	if err != nil {
		return timelinedomain.MediaFile{}, nil, err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	if _, err := io.Copy(f, body); err != nil {
		cleanup()
		return timelinedomain.MediaFile{}, nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return timelinedomain.MediaFile{}, nil, err
	}
	file := timelinedomain.MediaFile{Path: f.Name()}
	if info, err := mediadomain.ProbeStreams(ctx, f.Name()); err == nil {
		file.HasVideo, file.HasAudio, file.AudioChannels = info.HasVideo, info.HasAudio, info.AudioChannels
	} else {
		file.HasVideo = strings.HasPrefix(m.ContentType, "video/") || strings.HasPrefix(m.ContentType, "image/")
	}
	return file, func() { _ = os.Remove(f.Name()) }, nil
}

func mediaExt(mime string) string {
	switch mime {
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	}
	return ".bin"
}

type renderArchiver struct{ svc *mediaapp.Service }

func (a renderArchiver) Discard(ctx context.Context, workspaceID, mediaID int64) error {
	return a.svc.Delete(ctx, workspaceID, mediaID)
}

func (a renderArchiver) Archive(ctx context.Context, workspaceID, userID int64,
	mime string, size int64, body io.Reader) (int64, error) {
	up, err := a.svc.Upload(ctx, mediaapp.UploadInput{
		WorkspaceID: workspaceID, UserID: userID, Purpose: "render_output",
		DeclaredMIME: mime, Size: size, Body: body,
	})
	if err != nil {
		return 0, err
	}
	return up.ID, nil
}

type renderRefunder struct {
	repo     *billingrepo.Repo
	validFor time.Duration
}

func (a renderRefunder) Refund(ctx context.Context, payerID int64, refType, refID string, _ int, reason string) error {
	_, err := a.repo.Refund(ctx, billingrepo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID, Reason: reason, CompensationValidFor: a.validFor,
	})
	return err
}
