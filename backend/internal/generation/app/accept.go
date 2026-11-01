// Package app 编排生成受理。
//
// ⚠️ 受理是本系统事务边界最关键的一处（FLOW-01）。事务内**只做库内操作**：
//
//	幂等键 → 归属与配置快照 → 写 queued + 参数/输入快照 → 扣费 → 写 outbox
//
// **Provider 调用、对象存储上传、MQ publish 一律在事务外**：
// 事务里发远程请求会让一次上游卡顿变成一批数据库连接被长时间占用，
// 而且事务回滚也撤不回已经发出去的请求。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	billingdomain "github.com/li904338203/tabtv/backend/internal/billing/domain"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	"github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	imagetooldomain "github.com/li904338203/tabtv/backend/internal/imagetool/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// CreditReserver 由账本域实现：在**调用方的事务里**扣费。
type CreditReserver interface {
	ConsumeInTx(ctx context.Context, tx pgx.Tx, in billingrepo.ConsumeInput) (
		billingrepo.ConsumeResult, error)
}

// Quoter 由目录域实现：报价并做扣费前的全部校验（能力矩阵 → 参数 → 会员 → 计价）。
type Quoter interface {
	QuoteForAccept(ctx context.Context, in QuoteAsk) (QuoteAnswer, error)
}

type QuoteAsk struct {
	ModelCode     string
	OperationCode string
	Count         int
	DurationSec   int
	Text          string
	MemberLevelID int64
	RoleCounts    map[string]int
	// Params 本次生成的参数（清晰度、画幅……），报价据此在扣费前按协议范围校验（04 文档 P-61）。
	Params json.RawMessage
	// AppendixChars 是服务端追加段（营销片段 + 场景约束段）的码点数（MKT-T-21、MKD-17）。
	//
	// ⚠️ **只参与 `max_input_chars` 判定，不参与计价**：附加段同样发给上游，不算进去的话
	// 四段全选超过模型上限时受理照样通过、扣费、排队，上游才回 400；而 `per_kchar` 的
	// 文本模型不该因为平台加了约束段就多收用户的钱。
	AppendixChars int
}

type QuoteAnswer struct {
	Credits          int
	EstimatedSeconds int
	ResultType       string
	ModelID          int64
	OperationID      int64
	ProviderCode     string
	APIProtocol      string
	PriceUnit        string
	PriceCredits     int
}

// PresetAsk 是一次预设解析请求。
type PresetAsk struct {
	// Params 是节点参数，里面可能带 `preset` 与 `presetOptions`。
	Params json.RawMessage
	// HasImage / Text 用于校验预设的输入要求（image_required 等）。
	HasImage bool
	Text     string
}

// PresetSource 由目录域实现：按 `params.preset` 取配方并拼成提示词。
//
// ⚠️ 没选预设时返回空串而不是报错：绝大多数生成不走预设。
type PresetSource interface {
	PresetPrompt(ctx context.Context, ask PresetAsk) (string, error)
}

func (s *Service) SetPresets(p PresetSource) { s.presets = p }

// CameraSource 由目录域实现：按 `params.camera`（视频节点的「运镜」）拼一句提示词。
//
// ⚠️ 没选运镜时返回空串：绝大多数生成不带运镜。
type CameraSource interface {
	CameraPrompt(ctx context.Context, params json.RawMessage) (string, error)
}

func (s *Service) SetCamera(c CameraSource) { s.camera = c }

// MarketingAppendix 是营销段的拼接结果。
type MarketingAppendix struct {
	// Text 是「创意 → Hook → 风格 → 场景约束段」拼好的一段（营销域排好内部顺序）。
	Text string
	// Picks 是这次用到的钉住版本（`kind:code@version`），**只用于受理日志**。
	Picks []string
}

// MarketingSource 由营销域实现（wiring 适配，MKT-T-21、MKD-6）：
// 按 `params.marketing.picks` 的 (code, version) 取历史原文 + 场景约束段。
//
//   - `params.marketing` 不存在 → 空结果、不报错：**通用生成行为不变**（MKR-10）。
//   - 存在但一样没选 → 只有场景约束段（它是商品保真规则，不是选项的附属）。
//   - 钉住的 (code, version) 不存在 → InvalidArgument（只可能是手改 params）；
//     已下线 / 已过期的选项**照常拼**（钉住的版本就是用户确认过的配方）。
//
// ⚠️ 拼接**只有这一处**：Agent 工具、MCP、画布手动重跑、运维受控重试都经过受理，
// 不要在任何调用方再拼 —— 片段原文也因此从不进 canvasData（MK-8）。
type MarketingSource interface {
	Appendix(ctx context.Context, scope MarketingScope, params json.RawMessage) (MarketingAppendix, error)
}

// MarketingScope 是这次生成所在的工作区与项目：实现方据此判断「这些选项版本是不是本项目的营销会话
// 确认过的」（04 文档 P-46）—— 否则任何生成入口都能凭 (code, version) 取到任意选项的片段原文，
// 包括草稿与已下线的，再让文本模型复述一遍就能拿到原文。
type MarketingScope struct {
	WorkspaceID int64
	ProjectID   int64
}

// SetMarketing 接上营销段来源。⚠️ 在 wiring.BuildCore 里与 SetPresets 并列注入：
// API 与 Worker（Agent 的 generate 在 Worker 里调受理）共用这一处，只接一边的话
// 另一边发出的生成静默缺少营销片段 —— 不报错，只是效果差（MKR-11）。
func (s *Service) SetMarketing(m MarketingSource) { s.marketing = m }

// HasMarketing 供装配测试确认已注入（MKR-11）。
func (s *Service) HasMarketing() bool { return s.marketing != nil }

// InputResolver 由画布域实现：把声明的输入解析成可信、有序的清单。
//
// ⚠️ 它同时回**角色计数**：能力矩阵要按「这次真的传了几张图/几段视频」校验，
// 而这个数只有解析方知道 —— 客户端给的是 ID，服务端解析后才知道那是图还是视频。
// 让受理方自己去数解析结果，等于把画布域的 JSON 形状抄进生成域。
type InputResolver interface {
	ResolveForAccept(ctx context.Context, workspaceID, projectID int64, nodeID, operation string,
		declared json.RawMessage) (out ResolvedInputs, err error)
}

// NoProjectInputResolver 是不属于任何项目的任务的输入解析（MKT-T-35「生成角色」，dev/05 MKD-28）。
//
// 只有 domain.AllowsNoProject 的来源会走这里：参考图按**工作区**解析，`generationId` 输入一律拒绝
// （按项目比对的那条防线在 project_id = 0 时不成立 —— 任何别人的无项目任务都会「同项目」）。
// 由同一个画布域解析器实现（wiring.inputAdapter），规则一份。
type NoProjectInputResolver interface {
	ResolveForAcceptNoProject(ctx context.Context, workspaceID int64, operation string,
		declared json.RawMessage) (out ResolvedInputs, err error)
}

// ResolvedInputs 是解析结果。
type ResolvedInputs struct {
	Inputs       json.RawMessage
	PromptSuffix string
	// RoleCounts 各输入角色的实际数量（image/video/audio/text），供能力矩阵校验。
	RoleCounts map[string]int
	Issues     []string
	// DirectorKinds 是输入里 3D 导演台的产物种类（capture 截图 / preview 预演视频，按此顺序、去重），
	// 由画布域按媒体 purpose 汇总（DRD-18）。非空时受理追加导演台约束段。
	DirectorKinds []string
	// RangeLengthMs 是带区间的输入里最长那段的长度（毫秒，0 = 没有区间，docs/agent/smartcut SCD-34）。
	// 非 0 时按它向上取整到秒作为计价时长，覆盖客户端报的 durationSec。
	RangeLengthMs int
}

// DirectorGuardSource 由目录域实现：按导演台产物的种类渲染约束段（DRD-18，模板在 prompt_template，运营可调）。
//
// ⚠️ 约束段只靠用户自己写的话，忘写的代价是一次花钱的白模出图：模型照着灰色素模画出一张灰色的图。
type DirectorGuardSource interface {
	DirectorGuardPrompt(ctx context.Context, kinds []string) (string, error)
}

// SetDirectorGuard 接上导演台约束段来源。⚠️ 在 wiring.BuildCore 里与 SetMarketing 并列注入
// （API 与 Worker 共用）：只接一边的话，另一边发出的生成静默缺少约束段。
func (s *Service) SetDirectorGuard(d DirectorGuardSource) { s.director = d }

// HasDirectorGuard 供装配测试确认已注入。
func (s *Service) HasDirectorGuard() bool { return s.director != nil }

// MediaURLDelivery 回答「这个协议的上游收不收得到这边的参考媒体」（docs/agent/smartcut SCT-38）。
//
// 只收地址的协议（provider/protocol.MediaURLRequiring：去字幕、UniArt、火山识别标准版）在主存储签不出上游够得着的
// 地址、又没有公网中转时收不到。不问的话受理照样扣费、排队，Worker 在提交前判永久失败再退款 —— 用户白等一轮，
// 账本里多一对扣款与退款。由 provider/app.MediaDelivery 实现（判据与 Worker 发参考媒体时是同一条）。
type MediaURLDelivery interface {
	// Undeliverable 收不到时给原因（给用户看）；收得到、或者协议不要求地址时为空串。
	Undeliverable(ctx context.Context, apiProtocol string) string
}

// SetMediaURLDelivery 接上参考媒体的送达判断。⚠️ 在 wiring.BuildCore 里注入（API 与 Worker 共用：Agent 的生成
// 在 Worker 里受理）。不接时不检查，Worker 在提交前兜底。
func (s *Service) SetMediaURLDelivery(d MediaURLDelivery) { s.delivery = d }

type Service struct {
	// director 渲染 3D 导演台约束段（DRD-18，见 SetDirectorGuard）。
	director    DirectorGuardSource
	repo        *repo.Repo
	nextID      func() int64
	credits     CreditReserver
	quoter      Quoter
	inputs      InputResolver
	presets     PresetSource
	camera      CameraSource
	marketing   MarketingSource
	refunder    Refunder
	groupBinder GroupItemBinder
	runAdvancer RunAdvancer
	smartcut    SmartcutBinder
	events      EventPublisher
	delivery    MediaURLDelivery
	log         *slog.Logger
}

func New(r *repo.Repo, nextID func() int64) *Service {
	return &Service{repo: r, nextID: nextID}
}

func (s *Service) SetCredits(c CreditReserver)      { s.credits = c }
func (s *Service) SetQuoter(q Quoter)               { s.quoter = q }
func (s *Service) SetInputResolver(i InputResolver) { s.inputs = i }
func (s *Service) SetLogger(l *slog.Logger)         { s.log = l }

// SetEvents 接上状态事件。⚠️ 取消与运维补偿发生在 API 进程，不经过 Worker ——
// 只在 Worker 那侧接的话，这两条路进入终态时前端收不到任何推送。
func (s *Service) SetEvents(p EventPublisher) { s.events = p }

func (s *Service) logWarn(ctx context.Context, msg string, id int64, err error) {
	if s.log != nil {
		s.log.WarnContext(ctx, msg, "generationId", id, "error", err)
	}
}

// AcceptInput 是一次受理请求。
type AcceptInput struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ProjectID      int64
	NodeID         string
	OperationCode  string
	ModelCode      string
	IdempotencyKey string
	MemberLevelID  int64
	Count          int
	DurationSec    int
	// Text 是这次生成的描述文字：报价按它数字数、预设按它判断「有没有描述」。
	//
	// ⚠️ Worker **只从 `params.prompt`** 拼提示词，所以参数里没有 prompt 时受理会把 Text 写进去
	// （CD-81）。此前 Text 用完就丢、不进快照：只传了 Text 的调用方（组子项）发出去的请求里
	// 提示词是空的，而受理、扣费、排队一路成功，直到上游回 400。
	Text   string
	Params json.RawMessage
	// DeclaredInputs 是客户端声明的输入（**只有 ID，没有 URL**，见 CD-24）。
	DeclaredInputs json.RawMessage
	RoleCounts     map[string]int
	RetryOfID      int64
	// ScriptAssetID 非 0 表示这次任务是在给某个脚本资产出图，
	// 成功后要回写资产状态。
	ScriptAssetID int64
	// GroupItemID / ShotID 表示这次任务属于某个组的子项（P07-04）。
	GroupItemID int64
	ShotID      int64
	// InputFingerprint 由整图计划器给出，受理时原样存下来（P08-02）。
	// 其他入口留空 —— 留空就是"不可复用"，安全方向。
	InputFingerprint string
	// Origin 来源标识（domain.Origin*），冻结进快照。空 = 常规来源。
	// 只有 domain.AllowsNoProject 的来源允许 ProjectID = 0（MKT-T-35）。
	Origin string
	// SmartcutTarget 智能剪辑名下的任务完成后回写到哪里（`generation.smartcut_target`，docs/agent/smartcut）。
	// 非空时终态会交给 SmartcutBinder；只给 Origin = smartcut 的任务用。
	SmartcutTarget json.RawMessage
}

// AcceptResult 是受理结果。Replayed 为真表示这是幂等重放，没有新扣费。
type AcceptResult struct {
	Generation repo.Generation
	Replayed   bool
}

// Accept 受理一次生成。
func (s *Service) Accept(ctx context.Context, in AcceptInput) (AcceptResult, error) {
	if err := domain.ValidateIdempotencyKey(in.IdempotencyKey); err != nil {
		return AcceptResult{}, httpx.Errf(errcode.MissingIdempotencyKey, err.Error())
	}

	// ① 幂等重放：先查一次是为了**省掉后面整串工作**（报价、解析、扣费），
	// 不是为了保证唯一性 —— 唯一性由 uk_generation_idem 兜底，
	// 并发下两个请求都会查不到，然后一个成功一个撞索引。
	if existing, err := s.repo.FindByIdempotencyKey(ctx, in.PayerAccountID, in.IdempotencyKey); err == nil {
		return AcceptResult{Generation: existing, Replayed: true}, nil
	} else if !errors.Is(err, repo.ErrNotFound) {
		return AcceptResult{}, httpx.Wrap(errcode.Internal, err)
	}

	p, err := s.prepare(ctx, in)
	if err != nil {
		return AcceptResult{}, err
	}
	if p.quoteErr != nil {
		return AcceptResult{}, p.quoteErr
	}
	in.Params, in.OperationCode, in.Count = p.params, p.operation, p.count
	quote, resolvedInputs, promptSuffix := p.quote, p.inputs, p.suffix
	marketing := p.marketing

	snapshot, err := domain.BuildSnapshot(domain.Snapshot{
		ModelCode: in.ModelCode, ModelID: quote.ModelID, OperationCode: in.OperationCode,
		ProviderCode: quote.ProviderCode, APIProtocol: quote.APIProtocol,
		ResultType: quote.ResultType, PriceUnit: quote.PriceUnit,
		PriceCredits: quote.PriceCredits, CreditsCost: quote.Credits,
		EstSeconds: quote.EstimatedSeconds, PromptSuffix: promptSuffix,
		Origin: in.Origin,
	})
	if err != nil {
		return AcceptResult{}, httpx.Errf(errcode.Internal, err.Error())
	}

	// ④ 一个事务：写任务 + 扣费 + outbox。
	genID := s.nextID()
	deadline := domain.DeadlineAt(time.Now(), quote.EstimatedSeconds)
	g := repo.Generation{
		ID: genID, WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID,
		CreatedByUserID: in.UserID, ProjectID: in.ProjectID, NodeID: in.NodeID,
		OperationID: quote.OperationID, ModelID: quote.ModelID, ResultType: quote.ResultType,
		ParamsSnapshot: mergeParams(in.Params, snapshot), InputsSnapshot: resolvedInputs,
		RequestCount: maxInt(in.Count, 1), CreditsCost: quote.Credits,
		IdempotencyKey: in.IdempotencyKey, RetryOfID: in.RetryOfID,
		ScriptAssetID:    in.ScriptAssetID,
		GroupItemID:      in.GroupItemID,
		ShotID:           in.ShotID,
		InputFingerprint: in.InputFingerprint,
		SmartcutTarget:   in.SmartcutTarget,
		Status:           domain.StatusQueued, DeadlineAt: &deadline,
	}

	tx, err := s.repo.Pool().Begin(ctx)
	if err != nil {
		return AcceptResult{}, httpx.Wrap(errcode.Internal, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.repo.InsertInTx(ctx, tx, g); err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			// 并发重复提交：另一个请求已经写入。回滚本次（连同它的扣费），
			// 返回那个已经成功的任务 —— 重复提交不该产生第二次扣费。
			_ = tx.Rollback(ctx)
			existing, e2 := s.repo.FindByIdempotencyKey(ctx, in.PayerAccountID, in.IdempotencyKey)
			if e2 != nil {
				return AcceptResult{}, httpx.Errf(errcode.Conflict, "并发受理冲突，请重试")
			}
			return AcceptResult{Generation: existing, Replayed: true}, nil
		}
		return AcceptResult{}, httpx.Wrap(errcode.Internal, err)
	}

	if quote.Credits > 0 {
		if s.credits == nil {
			return AcceptResult{}, httpx.Errf(errcode.Internal, "账本未接入")
		}
		// ⚠️ ref_type + ref_id 分两列：退款要按同一对引用找回这笔消费。
		if _, err := s.credits.ConsumeInTx(ctx, tx, billingrepo.ConsumeInput{
			PayerID: in.PayerAccountID, Amount: quote.Credits,
			RefType: "generation", RefID: itoa(genID), UserID: in.UserID,
			Reason: in.OperationCode,
		}); err != nil {
			return AcceptResult{}, translateCredit(err)
		}
	}

	payload, err := json.Marshal(domain.OutboxPayload{
		GenerationID: itoa(genID), WorkspaceID: itoa(in.WorkspaceID),
		PayerAccountID: itoa(in.PayerAccountID), OperationCode: in.OperationCode,
		ModelCode: in.ModelCode, SchemaVersion: 1,
	})
	if err != nil {
		return AcceptResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if err := s.repo.InsertOutboxInTx(ctx, tx, s.nextID(), "generation", genID,
		domain.OutboxTopic, payload); err != nil {
		return AcceptResult{}, httpx.Wrap(errcode.Internal, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return AcceptResult{}, httpx.Wrap(errcode.Internal, err)
	}

	if marketing.Text != "" && s.log != nil {
		// 受理日志带钉住的版本（`kind:code@version`），排障时按它找回当时的片段原文。
		// ⚠️ 只进日志字段，不进指标 label（code 是高基数）。
		s.log.InfoContext(ctx, "受理拼接营销片段", "generationId", genID,
			"marketingPicks", marketing.Picks,
			"appendixChars", utf8.RuneCountInString(marketing.Text))
	}

	created, err := s.repo.Get(ctx, in.WorkspaceID, genID)
	if err != nil {
		return AcceptResult{}, httpx.Wrap(errcode.Internal, err)
	}
	return AcceptResult{Generation: created}, nil
}

// prepared 是受理在写库之前的全部结论（输入解析 → 附加段 → 报价）。
type prepared struct {
	params     json.RawMessage
	inputs     json.RawMessage
	suffix     string
	marketing  MarketingAppendix
	roleCounts map[string]int
	quote      QuoteAnswer
	// quoteErr 是**报价阶段**的拒绝（能力矩阵 / 输入长度 / 会员 / 价格）。与输入解析的错误分开：
	// 预览（Preview）要把前者当「不可用 + 原因」回给用户，后者仍是请求错误。
	quoteErr error
	// operation / count 是服务端最终认定的操作码与张数（工具节点由 params.tool 定，不信客户端）。
	operation string
	count     int
}

// prepare 跑受理在扣费之前的每一步，**不写库**。Accept 与 Preview 共用这一份 ——
// 两份实现的话「预览说 12 积分、真生成扣 16」迟早出现，而两边各自看都"没错"（MKT-AC-39）。
func (s *Service) prepare(ctx context.Context, in AcceptInput) (prepared, error) {
	if !domain.ValidOrigin(in.Origin) {
		return prepared{}, httpx.Errf(errcode.Internal, "未知的任务来源："+in.Origin)
	}
	noProject := in.ProjectID == 0 && domain.AllowsNoProject(in.Origin)
	if domain.AllowsNoProject(in.Origin) && in.ProjectID != 0 {
		return prepared{}, httpx.Errf(errcode.Internal, "该来源的任务不属于任何项目")
	}
	// ①' 描述文字进参数（见 AcceptInput.Text）：不写进去就等于没发。
	in.Params = promptIntoParams(in.Params, in.Text)
	// ①'' 工具节点的操作码由 params.tool 定（IMGD-3），**在输入解析之前**换掉：解析要按真正的操作校验输入类型。
	toolCode := toolCodeOf(in.Params)
	if toolCode != "" {
		// 工具子节点一律图生图（R-5）：模型、价格、能力矩阵都按图生图走，不信客户端报的 operation。
		in.OperationCode = imagetooldomain.OpImg2Img
	}
	// upstream / output 只能由服务端写（IMGD-2）：不管是不是工具节点，客户端传来的一律丢掉。
	in.Params = stripServerOwned(in.Params)

	// ② 输入解析（可信 ID → 有序清单）。在报价之前：报价要按真实输入数量算。
	var resolvedInputs json.RawMessage = json.RawMessage("[]")
	var promptSuffix string
	var directorKinds []string
	roleCounts := in.RoleCounts
	if s.inputs != nil {
		out, err := s.resolveInputs(ctx, in, noProject)
		if err != nil {
			return prepared{}, err
		}
		if len(out.Issues) > 0 {
			// ⚠️ 输入不合法就**不要走到扣费**：等上游返回 400 才发现的话，
			// 积分已经扣了、任务已经排了队，退款和清理都是额外的补偿路径。
			return prepared{}, httpx.Errf(errcode.InvalidArgument, out.Issues[0]).
				WithDetails(map[string]any{"issues": out.Issues})
		}
		resolvedInputs, promptSuffix, directorKinds = out.Inputs, out.PromptSuffix, out.DirectorKinds
		// ⚠️ 角色计数以**服务端解析结果**为准，不信客户端传来的数。
		// 信客户端的话，能力矩阵的 min/max 等于没有 —— 少报一张图就能绕过去。
		roleCounts = out.RoleCounts
		// ⚠️ 带区间的输入：时长由服务端按区间算（向上取整到秒），不收客户端报的 durationSec（SCD-34）——
		// 信客户端的话，一段 60 秒的素材报成 1 秒就只付 1 秒的钱。
		if out.RangeLengthMs > 0 {
			in.DurationSec = (out.RangeLengthMs + 999) / 1000
		}
	}

	// ②⁰ 图片节点工具条（docs/agent/imagetools IMGD-2/3）：操作码由 params.tool 定，选项在这里校验、翻译、冻进快照。
	// ⚠️ 工具节点**不拼预设 / 素材 / 营销段**：工具的配方是固定的（翻译结果就是全部），
	// 拼进去的话一段「电影级光影」会跟着多角度一起发出去。
	var tool *imagetooldomain.Prepared
	if toolCode != "" {
		t, params, err := toolStep(toolCode, in.Params, resolvedInputs)
		if err != nil {
			return prepared{}, err
		}
		tool, in.Params, in.Count = &t, params, t.Count
	}

	// ②' 预设 + 导演台约束段 + ②'' 营销片段 → 拼进快照的附加段（见 composeSuffix）。
	var marketing MarketingAppendix
	var directorPart string
	if tool == nil {
		var err error
		promptSuffix, marketing, directorPart, err = s.composeSuffix(ctx, in, roleCounts, directorKinds, promptSuffix)
		if err != nil {
			return prepared{}, err
		}
	} else {
		promptSuffix = ""
	}

	// ③ 报价与能力校验。⚠️ **用服务端当前价，不信前端传来的费用**。
	if s.quoter == nil {
		return prepared{}, httpx.Errf(errcode.Internal, "报价服务未接入")
	}
	quote, quoteErr := s.quoter.QuoteForAccept(ctx, QuoteAsk{
		ModelCode: in.ModelCode, OperationCode: in.OperationCode,
		Count: in.Count, DurationSec: in.DurationSec, Text: in.Text,
		MemberLevelID: in.MemberLevelID, RoleCounts: roleCounts, Params: in.Params,
		// ⚠️ 只算营销追加段（MKD-17）与导演台约束段（DRD-18）：两者都是平台加的，不计入的话超长时报错里的字数
		// 对不上；预设与素材词的长度判定保持原样（通用生成行为不变）。
		AppendixChars: utf8.RuneCountInString(marketing.Text) + utf8.RuneCountInString(directorPart),
	})
	// ③' 只收地址的上游收不收得到参考媒体（SCT-38）：在扣费之前说清，不让 Worker 收了钱再退。
	if quoteErr == nil && s.delivery != nil && hasMediaRole(roleCounts) {
		if why := s.delivery.Undeliverable(ctx, quote.APIProtocol); why != "" {
			quoteErr = httpx.Errf(errcode.ModelUnavailable, why)
		}
	}

	return prepared{params: in.Params, inputs: resolvedInputs, suffix: promptSuffix, marketing: marketing,
		roleCounts: roleCounts, quote: quote, quoteErr: quoteErr, operation: in.OperationCode, count: in.Count}, nil
}

// hasMediaRole 输入里有没有媒体（图片、视频、音频、蒙版……）：纯文本的生成不发参考媒体，地址送不送得到与它无关。
func hasMediaRole(roleCounts map[string]int) bool {
	for role, n := range roleCounts {
		if role != "text" && n > 0 {
			return true
		}
	}
	return false
}

// resolveInputs 选输入解析入口：无项目来源走 NoProjectInputResolver，其余照旧。
func (s *Service) resolveInputs(ctx context.Context, in AcceptInput, noProject bool) (ResolvedInputs, error) {
	if !noProject {
		return s.inputs.ResolveForAccept(ctx, in.WorkspaceID, in.ProjectID,
			in.NodeID, in.OperationCode, in.DeclaredInputs)
	}
	np, ok := s.inputs.(NoProjectInputResolver)
	if !ok {
		return ResolvedInputs{}, httpx.Errf(errcode.Internal, "无项目任务的输入解析未接入")
	}
	return np.ResolveForAcceptNoProject(ctx, in.WorkspaceID, in.OperationCode, in.DeclaredInputs)
}

// PreviewResult 是一次不写库的受理预演。
type PreviewResult struct {
	Quote      QuoteAnswer
	RoleCounts map[string]int
	// QuoteErr 非空表示报价阶段拒绝（错误与 POST /generations/estimate 对同参请求给的是同一个）。
	QuoteErr error
}

// Preview 按受理的同一条路径（输入解析 → 附加段 → 报价）预演一次，**不写库、不扣费、不要幂等键**。
//
// 供「报价与真生成同一入口」的调用方用（营销「生成角色」的 dryRun，MKT-T-35）：
// 角色计数以服务端解析结果为准，与真受理一致。输入解析失败（参考图不属于本工作区等）作为 error 返回。
func (s *Service) Preview(ctx context.Context, in AcceptInput) (PreviewResult, error) {
	p, err := s.prepare(ctx, in)
	if err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{Quote: p.quote, RoleCounts: p.roleCounts, QuoteErr: p.quoteErr}, nil
}

// composeSuffix 拼受理快照里的附加段：`预设（含运镜）→ 导演台约束段 → 营销片段 → 素材词`
// （materialPart 是输入解析给的素材词）。返回营销段与导演台段本身，供报价算长度（MKD-17）与受理日志用。
func (s *Service) composeSuffix(ctx context.Context, in AcceptInput, roleCounts map[string]int,
	directorKinds []string, materialPart string) (string, MarketingAppendix, string, error) {
	// ②' 图片预设：把配方拼进提示词（NEW-12）。
	//
	// ⚠️ 拼接**在服务端**：前端拼出来的词会落进 canvasData，
	// 用户改一个字玩法就失效，历史节点也永远停在旧词，调词没法只靠改配置完成。
	//
	// ⚠️ 顺序是 `用户描述 → 预设版式 → 维度微调 → 营销片段 → 素材词`：
	// 识别信息必须排在审美词前，反过来模型会先照着风格词画，
	// 把用户真正要的内容挤掉。用户描述由 Provider 侧拼在最前，
	// 这里负责后三段的先后。
	//
	// ⚠️ 校验在**扣费之前**：不校验的话用户对一个空节点点"打光"，
	// 钱扣了、上游收到一句没有图的提示词，出来一张跟他的照片毫无关系的图。
	var presetPart string
	if s.presets != nil {
		part, err := s.presets.PresetPrompt(ctx, PresetAsk{
			Params:   in.Params,
			HasImage: roleCounts["image"] > 0,
			// ⚠️ 描述要同时看 `text` 与 `params.prompt`：图片节点的描述
			// **落在 params 里**（Provider 侧也是从那里读的），
			// 只看顶层 text 的话，用户明明写了词却被判成"没有描述"。
			Text: firstNonEmpty(in.Text, paramString(in.Params, "prompt")),
		})
		if err != nil {
			return "", MarketingAppendix{}, "", err
		}
		presetPart = part
	}

	// ②'+ 运镜（视频节点）：与预设同属「怎么拍」，紧跟在预设之后、营销片段与素材词之前 ——
	// 镜头运动、景别是对画面结构的要求，排在审美词后面的话会被风格词盖过去。
	// 词在服务端拼（选项与说法在 param_def.camera 的配置里），节点 params 里只存选了哪一项。
	var cameraPart string
	if s.camera != nil {
		part, err := s.camera.CameraPrompt(ctx, in.Params)
		if err != nil {
			return "", MarketingAppendix{}, "", err
		}
		cameraPart = part
	}
	if cameraPart != "" {
		presetPart = strings.TrimSpace(strings.Join([]string{strings.TrimSpace(presetPart), cameraPart}, "\n"))
	}

	// ②'' 营销片段（MKT-T-21）：只按钉住的 (code, version) 取历史原文 + 场景约束段。
	// 节点 params 里只有 `{kind, code, version}`，原文在这里取、拼好冻结进快照。
	var marketing MarketingAppendix
	if s.marketing != nil {
		m, err := s.marketing.Appendix(ctx, MarketingScope{WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID},
			in.Params)
		if err != nil {
			return "", MarketingAppendix{}, "", err
		}
		marketing = m
	}
	// ②''' 3D 导演台约束段（DRD-18）：输入里有导演台截图 / 预演视频时追加。排在预设（含运镜）之后、营销之前：
	// 预设讲「保持构图、出真实画面」，约束段讲「不采用白模外观」，两者都是对画面结构的要求，要压过审美词。
	var directorPart string
	if len(directorKinds) > 0 {
		if s.director == nil {
			// ⚠️ 不静默少一段：少了它模型照着灰色素模画，而任务是「成功」的。
			return "", MarketingAppendix{}, "", httpx.Errf(errcode.Internal, "导演台约束段来源未接入")
		}
		part, err := s.director.DirectorGuardPrompt(ctx, directorKinds)
		if err != nil {
			return "", MarketingAppendix{}, "", err
		}
		directorPart = part
	}
	// ⚠️ 没有预设来源、也没有营销段与导演台段时**原样保留**素材段：通用生成的快照必须与改动前
	// 逐字相等（MKR-10），而 OrderAppendix 会去掉两端空白。
	if s.presets == nil && cameraPart == "" && marketing.Text == "" && directorPart == "" {
		return materialPart, marketing, "", nil
	}
	return domain.OrderAppendix(presetPart, directorPart, marketing.Text, materialPart), marketing, directorPart, nil
}

func (s *Service) Get(ctx context.Context, workspaceID, id int64) (repo.Generation, error) {
	g, err := s.repo.Get(ctx, workspaceID, id)
	if errors.Is(err, repo.ErrNotFound) {
		return g, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return g, httpx.Wrap(errcode.Internal, err)
	}
	return g, nil
}

func (s *Service) List(ctx context.Context, workspaceID, projectID int64, limit int,
	cursorID int64) ([]repo.Generation, bool, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.repo.ListByWorkspace(ctx, workspaceID, projectID, limit+1, cursorID)
	if err != nil {
		return nil, false, httpx.Wrap(errcode.Internal, err)
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// mergeParams 把用户参数与配置快照合成一个 params_snapshot。
//
// ⚠️ 用户参数**原样保留**（未知字段一起带走，GO-35）；快照放在 `_snapshot` 键下，
// 两者不混在一层 —— 混在一层的话，用户参数里恰好有个 modelCode 就会覆盖掉快照。
func mergeParams(userParams, snapshot json.RawMessage) json.RawMessage {
	var params map[string]json.RawMessage
	if len(userParams) > 0 {
		_ = json.Unmarshal(userParams, &params)
	}
	if params == nil {
		params = map[string]json.RawMessage{}
	}
	params["_snapshot"] = snapshot
	out, err := json.Marshal(params)
	if err != nil {
		return snapshot
	}
	return out
}

// translateCredit 把账本的领域错误翻成业务错误码。
//
// ⚠️ 受理走的是账本的**仓储**层（扣费必须在受理的事务里），
// 因此拿到的是 domain.ErrInsufficient 这类原始错误，而不是账本 app 层
// 已经翻译过的业务错误。不翻译的话，「积分不足」会以 500 的面目出现 ——
// 用户以为系统坏了，而这本是一个他能自己解决的问题。
func translateCredit(err error) error {
	var biz *httpx.BizError
	if errors.As(err, &biz) {
		return err
	}
	switch {
	case errors.Is(err, billingdomain.ErrInsufficient):
		return httpx.Errf(errcode.InsufficientCredits, err.Error())
	case errors.Is(err, billingrepo.ErrDuplicate):
		// 同一 generation 已扣过费：受理侧不该走到这里（幂等键先拦住了），
		// 真到了说明有人用不同幂等键指向同一个 refId。
		return httpx.Errf(errcode.Conflict, "该任务已扣过费")
	case errors.Is(err, billingdomain.ErrAmountInvalid), errors.Is(err, billingdomain.ErrOverflow):
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return httpx.Wrap(errcode.Internal, err)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// paramString 从参数 JSON 里读一个字符串字段。
// promptIntoParams 在参数里还没有 prompt 时把 Text 写进去（CD-81）。
//
// ⚠️ 已有 prompt 时不动：调用方放在参数里的那段（如脚本域拼好的提示词）才是要发的。
// 参数解不开时原样返回 —— 这里只补一个键，不新增一种失败。
func promptIntoParams(params json.RawMessage, text string) json.RawMessage {
	if strings.TrimSpace(text) == "" || strings.TrimSpace(paramString(params, "prompt")) != "" {
		return params
	}
	values := map[string]json.RawMessage{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &values); err != nil {
			return params
		}
	}
	if values == nil {
		values = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(text)
	if err != nil {
		return params
	}
	values["prompt"] = raw
	out, err := json.Marshal(values)
	if err != nil {
		return params
	}
	return out
}

func paramString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
