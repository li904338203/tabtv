package app

import (
	"context"
	"encoding/json"
	"time"
)

// 只读工具的端口（MCPT-04，开发 01 §4）。实现在 internal/wiring/mcp_read.go。
//
// ⚠️ 端口收的是 **Principal**，不是裸的 workspaceID：租户条件与角色都取自 Principal
// （凭据所在工作区 + 那一刻实时查出的角色），适配器把 Principal.Role 原样交给各域 ——
// 不像 Agent 的画布适配器那样写死 viewer（MCP 的角色就是用户本人的角色）。
// ⚠️ 这里的类型是 MCP 自己的，不出现任何域的 repo 类型：工具层只认这些。
// ⚠️ 只读端口**不得**触发任何扣费调用（报价是只读的，不算扣费）。

// Account 是 whoami 要的账号信息（身份 + 工作区 + 余额 + 会员）。
type Account interface {
	Describe(ctx context.Context, p Principal) (AccountInfo, error)
}

// AccountInfo 账号概况。用户文本（昵称、工作区名）由工具层标成不可信。
type AccountInfo struct {
	Nickname      string
	WorkspaceName string
	WorkspaceKind string // personal | team
	// Balance 是付费主体的当前可用积分（账本推导，整数）。
	Balance int
	// MemberLevelName 为空表示无会员；MemberExpireAt 同时为 nil。
	MemberLevelName string
	MemberExpireAt  *time.Time
}

// ProjectQuery 是项目列表的筛选（复用 AGT-45 的 kind 与服务端游标）。
type ProjectQuery struct {
	Kind    string // "" | canvas | agent
	Keyword string
	Limit   int
	Cursor  string
}

// ProjectSummary 是列表项（不含画布内容）。
type ProjectSummary struct {
	ID        int64
	Name      string
	Kind      string // canvas | agent
	Version   int
	CoverID   int64 // 展示封面的媒体 ID，0 = 无
	CreatedAt time.Time
	UpdatedAt time.Time
	// AgentState 最近活跃的 Agent 会话状态（只有 agent 项目有）。
	AgentState string
}

// ProjectPage 是一页项目。
type ProjectPage struct {
	Items      []ProjectSummary
	NextCursor string
}

// ProjectDetail 是单个项目：概况 + 画布规模（不含 canvasData 原文）。
type ProjectDetail struct {
	ProjectSummary
	NodeCount int
	EdgeCount int
}

// Projects 是项目读端口（canvas 域）。
type Projects interface {
	List(ctx context.Context, p Principal, q ProjectQuery) (ProjectPage, error)
	Get(ctx context.Context, p Principal, id int64) (ProjectDetail, error)
}

// CanvasNode 是画布大纲里的一个节点（canvas/domain.Outline 的投影）。
type CanvasNode struct {
	ID       string
	Type     string
	Title    string
	Status   string
	ByAgent  bool
	Upstream []string
	Params   json.RawMessage
}

// NodeTask 是节点最近一次任务（任意状态）与第一个产物。
type NodeTask struct {
	GenerationID  int64
	Status        string // generation 原始状态
	ErrorCode     string
	OutputMediaID int64
	OutputType    string
}

// CanvasSnapshot 是读画布的结果：大纲 + 每节点最近任务。
type CanvasSnapshot struct {
	ProjectID int64
	Version   int
	Nodes     []CanvasNode
	EdgeCount int
	Latest    map[string]NodeTask
}

// Canvas 是画布读端口（大纲在 canvas 域、最近任务在 generation 域，开发 01 §12 R10）。
type Canvas interface {
	Read(ctx context.Context, p Principal, projectID int64) (CanvasSnapshot, error)
}

// DocumentSummary 是项目文档列表项（不带正文）。
type DocumentSummary struct {
	ID        int64
	Path      string
	Version   int
	Source    string
	Bytes     int
	UpdatedAt time.Time
}

// Document 是项目文档全文。
type Document struct {
	DocumentSummary
	Content    string
	EditorName string
}

// Documents 是项目文档读端口（canvas 域 project_document）。
type Documents interface {
	List(ctx context.Context, p Principal, projectID int64) ([]DocumentSummary, error)
	// Get 按 docID 取；docID 为 0 时按 path 取。
	Get(ctx context.Context, p Principal, projectID, docID int64, path string) (Document, error)
}

// ModelFilter 是模型目录的筛选。Code 非空时只取这一个。
type ModelFilter struct {
	ResultType string
	Operation  string
	Code       string
}

// ModelView 是模型与其各操作的可用性（不可用的照样列出并写原因，MCP-F-27）。
type ModelView struct {
	Code             string
	Name             string
	Domain           string
	Status           string
	MinMemberLevelID int64
	MaxDurationSec   *int
	Available        bool
	BlockedCode      string
	BlockedReason    string
	Operations       []ModelOperationView
}

// ModelOperationView 是模型上一个操作的可用性与单价（服务端报价）。
type ModelOperationView struct {
	Code             string
	Domain           string
	Available        bool
	BlockedCode      string
	BlockedReason    string
	UserFixable      bool
	Credits          int
	Unit             string
	EstimatedSeconds int
}

// ImageToolQuery 是图片工具栏的上下文（当前节点连着什么）。
type ImageToolQuery struct {
	ModelCode  string
	ImageCount int
	HasText    bool
}

// ImageToolView 是工具栏上的一项（catalog.ImageTool 的投影）。
type ImageToolView struct {
	Code             string
	Name             string
	PresetGroup      string
	Spec             string
	InputMode        string
	Operation        string
	Available        bool
	BlockedCode      string
	BlockedReason    string
	UserFixable      bool
	Credits          int
	EstimatedSeconds int
}

// Catalog 是目录读端口（catalog 域）。会员等级取 Principal.MemberLevelID。
type Catalog interface {
	Models(ctx context.Context, p Principal, f ModelFilter) ([]ModelView, error)
	ImageTools(ctx context.Context, p Principal, q ImageToolQuery) ([]ImageToolView, error)
}

// TaskFilter 是任务列表的筛选（复用任务中心的四类 union，CD-62）。
type TaskFilter struct {
	Kind     string // "" | generation | workflow | render | export
	State    string // 任务中心的统一状态，"" 不筛
	Limit    int
	CursorAt *time.Time
	CursorID int64
}

// TaskItem 是任务列表的一项。State 是任务中心的统一状态（已过 NormalizeState）。
type TaskItem struct {
	Kind      string
	ID        int64
	State     string
	Title     string
	Credits   int
	Error     string
	CreatedAt time.Time
	RefID     int64
}

// TaskPage 是一页任务与各统一状态的计数（计数与列表同一套条件）。
type TaskPage struct {
	Items  []TaskItem
	Counts map[string]int
}

// TaskOutput 是任务的一个产物。
type TaskOutput struct {
	Type    string
	MediaID int64
	Text    string
}

// TaskNode 是整图运行里的一个节点。
type TaskNode struct {
	NodeID       string
	Status       string
	GenerationID int64
	Reused       bool
	ErrorCode    string
}

// TaskDetail 是单个任务（四类之一）。RawStatus 是该域自己的原始状态。
type TaskDetail struct {
	Kind         string
	ID           int64
	RawStatus    string
	Progress     int
	Stage        string
	Credits      int
	ErrorCode    string
	ErrorMessage string
	ProjectID    int64
	NodeID       string
	CreatedAt    *time.Time
	UpdatedAt    *time.Time
	ExpiresAt    *time.Time
	Outputs      []TaskOutput
	Nodes        []TaskNode
}

// Tasks 是任务读端口（list：content 任务中心；get：按类分派到 generation / workflow / timeline / media）。
type Tasks interface {
	List(ctx context.Context, p Principal, f TaskFilter) (TaskPage, error)
	Get(ctx context.Context, p Principal, kind string, id int64) (TaskDetail, error)
}

// MediaInfo 是媒体元信息（不含 bucket / objectKey）。
type MediaInfo struct {
	ID          int64
	ContentType string
	SizeBytes   int64
	Width       int
	Height      int
	DurationMs  int
	Purpose     string
}

// Media 是媒体读端口（media 域）。每个方法都先在 SQL 里校验归属：不存在与不是你的**不区分**（NOT_FOUND）。
type Media interface {
	Info(ctx context.Context, p Principal, mediaID int64) (MediaInfo, error)
	// SignedURL 签发短期直链，返回过期时刻。
	SignedURL(ctx context.Context, p Principal, mediaID int64) (string, time.Time, error)
	// Thumbnail 取 320px 缩略图字节（只对位图调用；调用方负责判断类型）。
	Thumbnail(ctx context.Context, p Principal, mediaID int64) ([]byte, string, error)
}
