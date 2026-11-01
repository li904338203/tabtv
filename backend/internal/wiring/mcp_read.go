package wiring

// 对外 MCP 只读工具（MCPT-04）的端口适配器：只做类型转换，业务判断都在各域服务里。
//
// ⚠️ 角色一律传 **Principal.Role**（用户本人此刻在该工作区的角色），不照抄 AgentCanvas.Read 的
// RoleViewer —— Agent 的会话发起人角色由 Runner 另行校验，MCP 没有那一层。
// ⚠️ 租户条件一律取 Principal.WorkspaceID（凭据所在工作区），从不接受请求里给的工作区。

import (
	"context"
	"io"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasdomain "github.com/li904338203/tabtv/backend/internal/canvas/domain"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	contentapp "github.com/li904338203/tabtv/backend/internal/content/app"
	directorapp "github.com/li904338203/tabtv/backend/internal/director/app"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	mcptools "github.com/li904338203/tabtv/backend/internal/mcp/tools"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediadomain "github.com/li904338203/tabtv/backend/internal/media/domain"
	providertransport "github.com/li904338203/tabtv/backend/internal/provider/transport"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	workflowapp "github.com/li904338203/tabtv/backend/internal/workflow/app"
)

// MCPReadInput 是只读工具需要的各域服务。
type MCPReadInput struct {
	Identity *identityapp.Service
	Collab   *collabapp.Service
	Billing  *billingapp.Service
	Canvas   *canvasapp.Service
	GenRepo  *genrepo.Repo
	Gen      *genapp.Service
	Catalog  *catalogapp.Service
	Content  *contentapp.Service
	Workflow *workflowapp.Service
	Timeline *timelineapp.Service
	Media    *mediaapp.Service
	// MediaURLTTL 是 media.get 签名直链的有效期（SEEAITV_MCP_MEDIA_URL_TTL）。
	MediaURLTTL time.Duration
	// 报价（MCPT-05）：Store 为 nil 时不接入 quote（工具回「端口未接入」）。
	// QuoteTTL 是 quoteId 有效期（SEEAITV_MCP_QUOTE_TTL）；NextID 发 quoteId。
	Store    *mcprepo.Repo
	NextID   func() int64
	QuoteTTL time.Duration
	// generate（MCPT-10）：Ledger 为 nil 时不接入（工具回「端口未接入」）。StateKey 是 elicitation
	// requestState 的 HMAC 子密钥（mcpapp.DeriveStateKey(SEEAITV_SECRET_KEY)），为空时只剩基线确认。
	Ledger   *billingrepo.Repo
	StateKey []byte
	// ApprovalBase 是页面审批链接的用户端地址（MCPT-13；SEEAITV_WEB_PUBLIC_URL，开发下未配置时由 cmd/api 回落）。
	ApprovalBase string
	// 写工具（MCPT-11）。Download 是 media.upload 的 URL 下载客户端（共享出网策略；nil 时 URL 上传回「未接入」，
	// base64 照常）；WaitMax 是 tasks.wait 上限（SEEAITV_MCP_WAIT_MAX）。
	Download *providertransport.Client
	WaitMax  time.Duration
	// 多段工具集（MCPT-16）：脚本线 / 整图 / 时间线委托 AGT-50 的 Agent 适配器；Skills 是站内技能
	// （agent/app.Service 满足），为 nil 时不提供提示词。脚本线要画布编排，所以同 generate 一样只在 Ledger 非空时接入。
	Script     *scriptapp.Service
	ScriptRepo *scriptrepo.Repo
	Skills     SkillLister
	// 委托站内 Agent（MCPT-17）：Agent 为 nil 或 AgentEnabled=false（SEEAITV_AGENT_ENABLED 关）时 agent 工具不注册。
	// 会话链接的用户端地址与审批链接同一个（ApprovalBase = SEEAITV_WEB_PUBLIC_URL）。
	Agent        *agentapp.Service
	AgentEnabled bool
	// SmartcutAIEnabled=false（SEEAITV_SMARTCUT_AI_ENABLED 关）时 smartcut_plan 不注册。
	SmartcutAIEnabled bool
	// 3D 导演台（DRT-29）：AI 搭景服务（BuildCore 装的那一份）。为 nil 时 director_create 照常可用，
	// director_build 回「AI 搭景链路未接入」。
	Director *directorapp.Service
	// 智能剪辑（SCT-41）：带 AI 剪辑端口的那一份（cmd/api 装的）。为 nil 时四个智能剪辑工具回「端口未接入」。
	Smartcut *smartcutapp.Service
}

// NewMCPToolDeps 装出 MCP 工具的全部端口（cmd/api 用；快照生成器传零值 Deps）。
func NewMCPToolDeps(in MCPReadInput) mcptools.Deps {
	d := mcptools.Deps{
		Account:   MCPAccount{Identity: in.Identity, Collab: in.Collab, Billing: in.Billing},
		Projects:  MCPProjects{Svc: in.Canvas},
		Canvas:    MCPCanvas{Svc: in.Canvas, GenRepo: in.GenRepo},
		Documents: MCPDocuments{Svc: in.Canvas},
		Catalog:   MCPCatalog{Svc: in.Catalog},
		Tasks: MCPTasks{Content: in.Content, Gen: in.Gen, Workflow: in.Workflow, Timeline: in.Timeline,
			Media: in.Media},
		Media:  MCPMedia{Svc: in.Media, TTL: in.MediaURLTTL},
		Quotes: newMCPQuotes(in),
		// 写工具（MCPT-11）：全部调各域现有服务。
		TaskControl: MCPTaskControl{Tasks: MCPTasks{Content: in.Content, Gen: in.Gen, Workflow: in.Workflow,
			Timeline: in.Timeline, Media: in.Media}, Gen: in.Gen, Workflow: in.Workflow, Timeline: in.Timeline},
		Uploader:       MCPUploader{Svc: in.Media, Client: in.Download},
		CanvasWriter:   MCPCanvasWriter{Svc: in.Canvas},
		DocWriter:      MCPDocWriter{Svc: in.Canvas},
		ProjectCreator: MCPProjectCreator{Svc: in.Canvas},
		WaitMax:        in.WaitMax,
	}
	if in.Store != nil && in.NextID != nil && in.Ledger != nil {
		orch := NewCanvasGen(CanvasGenInput{Canvas: in.Canvas, Catalog: in.Catalog, Gen: in.Gen, Billing: in.Ledger})
		d.CanvasGen = MCPCanvasGen{
			Orch:    orch,
			Canvas:  in.Canvas,
			GenRepo: in.GenRepo,
		}
		if in.Script != nil {
			d.Scripts = MCPScripts{Agent: AgentScripts{Svc: in.Script, Repo: in.ScriptRepo,
				Quoter: AgentGenQuoter{Catalog: in.Catalog, Canvas: in.Canvas}}, Orch: orch}
		}
		confirm := mcpapp.NewConfirm(in.Store, MCPQuoter{Catalog: in.Catalog, Billing: in.Billing}, in.NextID,
			in.QuoteTTL, in.StateKey)
		confirm.SetApprovalBase(in.ApprovalBase)
		d.Confirm = confirm
	}
	if in.Canvas != nil {
		// 导演台不花钱：不像 generate / script 那样等 Ledger，只要画布（建节点、建场景、写回）。
		d.Directors = MCPDirectors{Orch: &canvasgen.Orchestrator{Canvas: canvasGenCanvas{in.Canvas}},
			Canvas: in.Canvas, Director: in.Director}
	}
	if in.Canvas != nil && in.Smartcut != nil && in.Timeline != nil {
		// 智能剪辑不花钱（导出免费，0 积分不进确认）：与导演台一样只要画布，不等 Ledger。
		d.Smartcuts = MCPSmartcuts{Orch: &canvasgen.Orchestrator{Canvas: canvasGenCanvas{in.Canvas}},
			Svc: in.Smartcut, Timeline: in.Timeline}
	}
	if in.Workflow != nil {
		d.Workflows = MCPWorkflows{Agent: AgentWorkflows{Svc: in.Workflow}}
	}
	if in.Timeline != nil {
		d.Timelines = MCPTimelines{Agent: AgentTimelines{Svc: in.Timeline}, Scripts: in.Script}
	}
	if in.Skills != nil {
		d.Skills = MCPSkills{Lister: in.Skills, Render: AgentSkillRenderer{}}
	}
	d.WebBase = in.ApprovalBase
	d.SmartcutAIDisabled = !in.SmartcutAIEnabled
	if in.Agent != nil && in.AgentEnabled {
		d.Agent = MCPAgentSessions{Svc: in.Agent}
	} else {
		d.AgentDisabled = true
	}
	return d
}

// newMCPQuotes 装出报价服务；没给 Store 时返回 nil 接口（不是装着 nil 指针的接口）。
func newMCPQuotes(in MCPReadInput) mcpapp.QuoteService {
	if in.Store == nil || in.NextID == nil {
		return nil
	}
	return mcpapp.NewQuotes(MCPQuoter{Catalog: in.Catalog, Billing: in.Billing}, in.Store, in.NextID, in.QuoteTTL)
}

// ---------------------------------------------------------------- whoami

// MCPAccount 账号概况：昵称（identity）、工作区（collaboration）、余额与会员（billing）。
type MCPAccount struct {
	Identity *identityapp.Service
	Collab   *collabapp.Service
	Billing  *billingapp.Service
}

func (a MCPAccount) Describe(ctx context.Context, p mcpapp.Principal) (mcpapp.AccountInfo, error) {
	var out mcpapp.AccountInfo
	// 昵称只是展示：取不到（Me 还要查个人工作区，异常数据下会失败）不让 whoami 整个失败 ——
	// 账号状态已由 Resolver 实时校验过，这里不再承担判定。
	nickname := ""
	if u, _, err := a.Identity.Me(ctx, p.UserID); err == nil {
		nickname = u.Nickname
	}
	w, err := a.Collab.Describe(ctx, p.WorkspaceID)
	if err != nil {
		return out, err
	}
	bal, err := a.Billing.Balance(ctx, p.PayerAccountID)
	if err != nil {
		return out, err
	}
	out = mcpapp.AccountInfo{Nickname: nickname, WorkspaceName: w.Name, WorkspaceKind: string(w.Kind),
		Balance: bal}
	// 会员取不到不让 whoami 失败（同 MemberLevelID 的降级取向）：余额与身份才是它的主要内容。
	if m, err := a.Billing.CurrentMembership(ctx, p.PayerAccountID); err == nil && m != nil {
		out.MemberLevelName = m.LevelName
		exp := m.ExpireAt
		out.MemberExpireAt = &exp
	}
	return out, nil
}

// ---------------------------------------------------------------- 项目、画布、文档

// MCPProjects 项目读端口（canvas/app.Service.List / Get）。
type MCPProjects struct{ Svc *canvasapp.Service }

func (a MCPProjects) List(ctx context.Context, p mcpapp.Principal, q mcpapp.ProjectQuery) (mcpapp.ProjectPage, error) {
	page, err := a.Svc.List(ctx, p.WorkspaceID, p.Role, canvasapp.ListInput{
		Keyword: q.Keyword, Limit: q.Limit, Cursor: q.Cursor, Kind: canvasrepo.ProjectKind(q.Kind)})
	if err != nil {
		return mcpapp.ProjectPage{}, err
	}
	out := mcpapp.ProjectPage{NextCursor: page.NextCursor, Items: make([]mcpapp.ProjectSummary, 0, len(page.Items))}
	for _, it := range page.Items {
		s := projectSummary(it)
		// 类型只有列表算得出来（AGT-45：有没有 agent_session，查询时算、不落列）。
		s.Kind = string(canvasrepo.KindCanvas)
		if it.Agent != nil {
			s.Kind, s.AgentState = string(canvasrepo.KindAgent), it.Agent.State
		}
		out.Items = append(out.Items, s)
	}
	return out, nil
}

func (a MCPProjects) Get(ctx context.Context, p mcpapp.Principal, id int64) (mcpapp.ProjectDetail, error) {
	pr, err := a.Svc.Get(ctx, p.WorkspaceID, id, p.Role)
	if err != nil {
		return mcpapp.ProjectDetail{}, err
	}
	nodes, edges := canvasdomain.Outline(pr.CanvasData)
	return mcpapp.ProjectDetail{ProjectSummary: projectSummary(pr), NodeCount: len(nodes), EdgeCount: edges}, nil
}

func projectSummary(p canvasrepo.Project) mcpapp.ProjectSummary {
	return mcpapp.ProjectSummary{ID: p.ID, Name: p.Name, Version: p.Version, CoverID: p.DisplayCover,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

// MCPCanvas 读画布：大纲在 canvas 域（Outline），每节点最近任务在 generation 域（LatestByNode）。
type MCPCanvas struct {
	Svc     *canvasapp.Service
	GenRepo *genrepo.Repo
}

func (a MCPCanvas) Read(ctx context.Context, p mcpapp.Principal, projectID int64) (mcpapp.CanvasSnapshot, error) {
	pr, err := a.Svc.Get(ctx, p.WorkspaceID, projectID, p.Role)
	if err != nil {
		return mcpapp.CanvasSnapshot{}, err
	}
	nodes, edges := canvasdomain.Outline(pr.CanvasData)
	out := mcpapp.CanvasSnapshot{ProjectID: pr.ID, Version: pr.Version, EdgeCount: edges,
		Nodes: make([]mcpapp.CanvasNode, 0, len(nodes)), Latest: map[string]mcpapp.NodeTask{}}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, mcpapp.CanvasNode{ID: n.ID, Type: n.Type, Title: n.Title, Status: n.Status,
			ByAgent: n.ByAgent, Upstream: n.Upstream, Params: n.Params})
	}
	// 项目已在上面按本工作区校验过；LatestByNode 自己仍带 workspace 条件（CD-04）。
	rows, err := a.GenRepo.LatestByNode(ctx, p.WorkspaceID, pr.ID)
	if err != nil {
		return mcpapp.CanvasSnapshot{}, httpx.Wrap(errcode.Internal, err)
	}
	for _, r := range rows {
		out.Latest[r.NodeID] = mcpapp.NodeTask{GenerationID: r.GenerationID, Status: r.Status,
			ErrorCode: r.ErrorCode, OutputMediaID: r.OutputMediaID, OutputType: r.OutputType}
	}
	return out, nil
}

// MCPDocuments 项目文档读端口（canvas/app 的 ListDocuments / GetDocument / DocumentByPath）。
type MCPDocuments struct{ Svc *canvasapp.Service }

func (a MCPDocuments) List(ctx context.Context, p mcpapp.Principal, projectID int64) ([]mcpapp.DocumentSummary, error) {
	list, err := a.Svc.ListDocuments(ctx, p.WorkspaceID, projectID, p.Role)
	if err != nil {
		return nil, err
	}
	out := make([]mcpapp.DocumentSummary, 0, len(list))
	for _, d := range list {
		out = append(out, mcpapp.DocumentSummary{ID: d.ID, Path: d.Path, Version: d.Version, Source: d.Source,
			Bytes: d.Bytes, UpdatedAt: d.UpdatedAt})
	}
	return out, nil
}

func (a MCPDocuments) Get(ctx context.Context, p mcpapp.Principal, projectID, docID int64,
	path string) (mcpapp.Document, error) {
	var d canvasrepo.Document
	var err error
	if docID != 0 {
		d, err = a.Svc.GetDocument(ctx, p.WorkspaceID, projectID, docID, p.Role)
	} else {
		d, err = a.Svc.DocumentByPath(ctx, p.WorkspaceID, projectID, p.Role, path)
	}
	if err != nil {
		return mcpapp.Document{}, err
	}
	return mcpapp.Document{
		DocumentSummary: mcpapp.DocumentSummary{ID: d.ID, Path: d.Path, Version: d.Version, Source: d.Source,
			Bytes: len(d.Content), UpdatedAt: d.UpdatedAt},
		Content: d.Content, EditorName: d.EditorName,
	}, nil
}

// ---------------------------------------------------------------- 目录

// MCPCatalog 目录读端口：模型可用性（ModelsWithAvailability）与图片工具栏（ImageTools）。
// 两者都走报价那条路径判可用性，会员等级取 Principal（实时解析）。
type MCPCatalog struct{ Svc *catalogapp.Service }

func (a MCPCatalog) Models(ctx context.Context, p mcpapp.Principal, f mcpapp.ModelFilter) ([]mcpapp.ModelView, error) {
	list, err := a.Svc.ModelsWithAvailability(ctx, f.ResultType, f.Operation, p.MemberLevelID)
	if err != nil {
		return nil, err
	}
	out := make([]mcpapp.ModelView, 0, len(list))
	for _, m := range list {
		if f.Code != "" && m.Code != f.Code {
			continue
		}
		mv := mcpapp.ModelView{Code: m.Code, Name: m.Name, Domain: m.Domain, Status: m.Status,
			MinMemberLevelID: m.MinMemberLevelID, MaxDurationSec: m.MaxDurationSec, Available: m.Available,
			BlockedCode: m.BlockedCode, BlockedReason: m.BlockedReason,
			Operations: make([]mcpapp.ModelOperationView, 0, len(m.Operations))}
		for _, o := range m.Operations {
			mv.Operations = append(mv.Operations, mcpapp.ModelOperationView{Code: o.Code, Domain: o.Domain,
				Available: o.Available, BlockedCode: o.BlockedCode, BlockedReason: o.BlockedReason,
				UserFixable: o.UserFixable, Credits: o.Credits, Unit: o.Unit, EstimatedSeconds: o.EstimatedSeconds})
		}
		out = append(out, mv)
	}
	return out, nil
}

func (a MCPCatalog) ImageTools(ctx context.Context, p mcpapp.Principal, q mcpapp.ImageToolQuery) (
	[]mcpapp.ImageToolView, error) {
	list, err := a.Svc.ImageTools(ctx, catalogapp.ImageToolAsk{ModelCode: q.ModelCode,
		MemberLevelID: p.MemberLevelID, ImageCount: q.ImageCount, HasText: q.HasText})
	if err != nil {
		return nil, err
	}
	out := make([]mcpapp.ImageToolView, 0, len(list))
	for _, t := range list {
		out = append(out, mcpapp.ImageToolView{Code: t.Code, Name: t.Name, PresetGroup: t.PresetGroup, Spec: t.Spec,
			InputMode: t.InputMode, Operation: t.Operation, Available: t.Available, BlockedCode: t.BlockedCode,
			BlockedReason: t.BlockedReason, UserFixable: t.UserFixable, Credits: t.Credits,
			EstimatedSeconds: t.EstimatedSeconds})
	}
	return out, nil
}

// ---------------------------------------------------------------- 任务

// MCPTasks 任务读端口：list 走任务中心的 union（content），get 按类分派到各自的域。
type MCPTasks struct {
	Content  *contentapp.Service
	Gen      *genapp.Service
	Workflow *workflowapp.Service
	Timeline *timelineapp.Service
	Media    *mediaapp.Service
}

func (a MCPTasks) List(ctx context.Context, p mcpapp.Principal, f mcpapp.TaskFilter) (mcpapp.TaskPage, error) {
	list, counts, err := a.Content.Tasks(ctx, p.WorkspaceID, f.Kind, f.State, f.Limit, f.CursorAt, f.CursorID)
	if err != nil {
		return mcpapp.TaskPage{}, err
	}
	out := mcpapp.TaskPage{Items: make([]mcpapp.TaskItem, 0, len(list)), Counts: map[string]int{}}
	for _, t := range list {
		out.Items = append(out.Items, mcpapp.TaskItem{Kind: string(t.Kind), ID: t.ID, State: string(t.State),
			Title: t.Title, Credits: t.Credits, Error: t.Error, CreatedAt: t.CreatedAt, RefID: t.RefID})
	}
	for k, v := range counts {
		out.Counts[string(k)] = v
	}
	return out, nil
}

func (a MCPTasks) Get(ctx context.Context, p mcpapp.Principal, kind string, id int64) (mcpapp.TaskDetail, error) {
	ws := p.WorkspaceID
	switch kind {
	case "generation":
		g, err := a.Gen.Get(ctx, ws, id)
		if err != nil {
			return mcpapp.TaskDetail{}, err
		}
		d := mcpapp.TaskDetail{Kind: kind, ID: g.ID, RawStatus: g.Status, Progress: g.Progress, Credits: g.CreditsCost,
			ErrorCode: g.ErrorCode, ErrorMessage: g.ErrorMessage, ProjectID: g.ProjectID, NodeID: g.NodeID,
			CreatedAt: timePtr(g.CreatedAt), UpdatedAt: timePtr(g.UpdatedAt)}
		outs, err := a.Gen.Outputs(ctx, ws, id)
		if err != nil {
			return mcpapp.TaskDetail{}, err
		}
		for _, o := range outs {
			d.Outputs = append(d.Outputs, mcpapp.TaskOutput{Type: o.OutputType, MediaID: o.MediaObjectID,
				Text: o.TextContent})
		}
		return d, nil
	case "workflow":
		v, err := a.Workflow.Run(ctx, ws, id)
		if err != nil {
			return mcpapp.TaskDetail{}, err
		}
		d := mcpapp.TaskDetail{Kind: kind, ID: v.Run.ID, RawStatus: v.Run.Status, Credits: v.Run.TotalCredits,
			ProjectID: v.Run.ProjectID}
		for _, n := range v.Nodes {
			d.Nodes = append(d.Nodes, mcpapp.TaskNode{NodeID: n.NodeID, Status: n.Status,
				GenerationID: n.GenerationID, Reused: n.IsReused, ErrorCode: n.ErrorCode})
		}
		return d, nil
	case "render":
		j, err := a.Timeline.GetRender(ctx, ws, id)
		if err != nil {
			return mcpapp.TaskDetail{}, err
		}
		d := mcpapp.TaskDetail{Kind: kind, ID: j.ID, RawStatus: j.Status, Progress: j.Progress, Stage: j.Stage,
			Credits: j.CreditsCost, ErrorCode: j.ErrorCode, ErrorMessage: j.ErrorMessage,
			CreatedAt: timePtr(j.CreatedAt), UpdatedAt: timePtr(j.UpdatedAt)}
		if j.OutputMediaID != 0 {
			d.Outputs = []mcpapp.TaskOutput{{Type: "video", MediaID: j.OutputMediaID}}
		}
		return d, nil
	case "export":
		t, err := a.Media.GetExport(ctx, ws, id)
		if err != nil {
			return mcpapp.TaskDetail{}, err
		}
		d := mcpapp.TaskDetail{Kind: kind, ID: t.ID, RawStatus: t.Status, ErrorCode: t.ErrorCode,
			ErrorMessage: t.ErrorMsg, CreatedAt: timePtr(t.CreatedAt), UpdatedAt: timePtr(t.UpdatedAt),
			ExpiresAt: timePtr(t.ExpiresAt)}
		// 过期是算出来的（CD-47）：包已清理就不给产物，阶段写明 expired。
		if mediadomain.EffectiveExportStatus(t.Status, t.ExpiresAt, time.Now()) == mediadomain.ExportExpired {
			d.Stage = mediadomain.ExportExpired
		} else if t.MediaID != 0 {
			d.Outputs = []mcpapp.TaskOutput{{Type: "zip", MediaID: t.MediaID}}
		}
		return d, nil
	}
	return mcpapp.TaskDetail{}, httpx.Errf(errcode.InvalidArgument, "未知任务类型 "+kind)
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ---------------------------------------------------------------- 媒体

// MCPMedia 媒体读端口：每一步都先在 SQL 里校验归属（Info / SignedURL / Download 都带工作区条件）。
type MCPMedia struct {
	Svc *mediaapp.Service
	TTL time.Duration
}

func (a MCPMedia) Info(ctx context.Context, p mcpapp.Principal, mediaID int64) (mcpapp.MediaInfo, error) {
	m, err := a.Svc.Info(ctx, p.WorkspaceID, mediaID)
	if err != nil {
		return mcpapp.MediaInfo{}, err
	}
	return mcpapp.MediaInfo{ID: m.ID, ContentType: m.ContentType, SizeBytes: m.SizeBytes, Width: m.Width,
		Height: m.Height, DurationMs: m.DurationMs, Purpose: m.Purpose}, nil
}

func (a MCPMedia) SignedURL(ctx context.Context, p mcpapp.Principal, mediaID int64) (string, time.Time, error) {
	return a.Svc.SignedURL(ctx, p.WorkspaceID, mediaID, a.TTL)
}

// maxThumbRead 读缩略图的上限：缩成功的话只有几十 KB，多读一点用来判断「没缩成（拿到的是原图）」。
const maxThumbRead = 1 << 20

func (a MCPMedia) Thumbnail(ctx context.Context, p mcpapp.Principal, mediaID int64) ([]byte, string, error) {
	rc, m, err := a.Svc.Download(ctx, p.WorkspaceID, mediaID, 320)
	if err != nil {
		return nil, "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxThumbRead+1))
	if err != nil {
		return nil, "", httpx.Wrap(errcode.Internal, err)
	}
	if len(b) > maxThumbRead {
		return nil, "", httpx.Errf(errcode.Internal, "缩略图过大")
	}
	return b, m.ContentType, nil
}

var (
	_ mcpapp.Account   = MCPAccount{}
	_ mcpapp.Projects  = MCPProjects{}
	_ mcpapp.Canvas    = MCPCanvas{}
	_ mcpapp.Documents = MCPDocuments{}
	_ mcpapp.Catalog   = MCPCatalog{}
	_ mcpapp.Tasks     = MCPTasks{}
	_ mcpapp.Media     = MCPMedia{}
)
