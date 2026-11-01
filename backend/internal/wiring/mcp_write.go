package wiring

// 对外 MCP 写工具（MCPT-11）的端口适配器：任务取消、参考文件上传、写画布、写文档、建项目。
// 只做类型转换与分派，业务判断都在各域服务里（取消的 CAS 与退款、上传的类型嗅探与上限、
// 画布的所有权与批次幂等、文档的乐观锁、项目的创建幂等）。
//
// ⚠️ 租户一律 Principal.WorkspaceID、角色一律 Principal.Role、操作者一律 Principal.UserID。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasdomain "github.com/li904338203/tabtv/backend/internal/canvas/domain"
	contentdomain "github.com/li904338203/tabtv/backend/internal/content/domain"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediadomain "github.com/li904338203/tabtv/backend/internal/media/domain"
	providertransport "github.com/li904338203/tabtv/backend/internal/provider/transport"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	workflowapp "github.com/li904338203/tabtv/backend/internal/workflow/app"
)

// ---------------------------------------------------------------- 取消

// MCPTaskControl 按类分派到各域**既有**的取消入口（MCP-F-55）：
//
//	generation → generation/app.Service.Cancel（CAS，连带组子项回写、整图推进、按原批次退款，只退一次）
//	workflow   → workflow/app.Service.Cancel（先标运行再取消子任务，子任务各自退款）
//	render     → timeline/app.Service.CancelRender（CAS，「本次是否真的改了状态」决定退不退）
//	export     → 没有取消入口（工具层已拒）
//
// ⚠️ 不碰任务表、不碰账本：在这里直接改状态的话就多了一条改任务状态的路径，退款 / 组子项 / 整图推进
// 总会漏一件（与 ops 域的人工补偿同一条，CD-63）。
// ⚠️ 已结束返回 Cancelled=false 而不是错误：模型连调两次取消、或者任务恰好刚跑完，都不是它做错了什么。
// 先查一次只是为了把「已结束」说清楚；真正的判定仍是各域的 CAS —— 查完到取消之间跑完的，取消入口
// 回 40905 / 40900，这里再查一次当前状态照样给 Cancelled=false。
type MCPTaskControl struct {
	Tasks    MCPTasks
	Gen      *genapp.Service
	Workflow *workflowapp.Service
	Timeline *timelineapp.Service
}

func (a MCPTaskControl) Cancel(ctx context.Context, p mcpapp.Principal, kind string, id int64,
	reason string) (mcpapp.CancelResult, error) {
	cur, err := a.Tasks.Get(ctx, p, kind, id)
	if err != nil {
		return mcpapp.CancelResult{}, err
	}
	if taskTerminal(cur) {
		return mcpapp.CancelResult{Task: cur}, nil
	}
	if reason == "" {
		reason = "外部 AI 客户端取消"
	}
	ws := p.WorkspaceID
	switch kind {
	case "generation":
		err = a.Gen.Cancel(ctx, ws, id, reason)
	case "workflow":
		_, err = a.Workflow.Cancel(ctx, ws, id, reason)
	case "render":
		_, err = a.Timeline.CancelRender(ctx, ws, id)
	default:
		return mcpapp.CancelResult{}, httpx.Errf(errcode.InvalidArgument, kind+" 任务不支持取消")
	}
	if err != nil && !raceFinished(err) {
		return mcpapp.CancelResult{}, err
	}
	now, gerr := a.Tasks.Get(ctx, p, kind, id)
	if gerr != nil {
		return mcpapp.CancelResult{}, gerr
	}
	st := contentdomain.NormalizeState(contentdomain.TaskKind(kind), now.RawStatus)
	// 是否「由本次取消」以取消之后的状态为准：整图的 Cancel 对已结束的运行不报错，只能看结果。
	return mcpapp.CancelResult{Cancelled: err == nil && st == contentdomain.StateCancelled, Task: now}, nil
}

// raceFinished：取消入口说「已经结束、取消不了」（生成 40905、渲染 409）—— 查完到取消之间跑完了。
func raceFinished(err error) bool {
	var be *httpx.BizError
	return errors.As(err, &be) && (be.Code == errcode.InvalidStateTransition || be.Code == errcode.Conflict)
}

func taskTerminal(t mcpapp.TaskDetail) bool {
	return contentdomain.TerminalState(contentdomain.NormalizeState(contentdomain.TaskKind(t.Kind), t.RawStatus))
}

// ---------------------------------------------------------------- 上传

// mcpUploadMaxBytes 是 URL 下载的上限：参考文件里最大的一类（视频 100 MB，media/domain.DefaultLimits）。
// 各类自己的上限由 media 域按实际类型再判一次。
var mcpUploadMaxBytes = mediadomain.DefaultLimits(mediadomain.KindVideo).MaxBytes

// MCPUploader 上传参考文件：统一调 media 域 Upload（Purpose=upload_reference，与用户端 POST /media/references
// 同一条路，按实际文件头判类型、走临时文件、探测尺寸与时长）。
//
// URL 来源用 Provider 的共享 HTTP 传输层下载（出网策略：production 禁内网；不跟随重定向；落临时文件不进内存；
// 用完 Close 即删）。⚠️ 不带任何认证头：这是用户给的任意地址，带上什么都可能泄漏给第三方（GO-31）。
type MCPUploader struct {
	Svc    *mediaapp.Service
	Client *providertransport.Client
	// Timeout 是 URL 下载的整体上限；0 取 40 秒（必须小于单次 tools/call 的 60 秒截止）。
	Timeout time.Duration
}

func (a MCPUploader) UploadReference(ctx context.Context, p mcpapp.Principal, src mcpapp.UploadSource) (
	mcpapp.Reference, error) {
	in := mediaapp.UploadInput{WorkspaceID: p.WorkspaceID, UserID: p.UserID, Purpose: "upload_reference",
		DeclaredMIME: src.DeclaredMIME, FileName: src.FileName}
	if src.URL != "" {
		if a.Client == nil {
			return mcpapp.Reference{}, httpx.Errf(errcode.Internal, "URL 上传未接入")
		}
		timeout := a.Timeout
		if timeout <= 0 {
			timeout = 40 * time.Second
		}
		dl, err := a.Client.Download(ctx, src.URL, nil, providertransport.Options{ConnectTimeout: 10 * time.Second,
			RequestTimeout: timeout}, mcpUploadMaxBytes)
		if err != nil {
			return mcpapp.Reference{}, downloadError(err)
		}
		defer dl.Close()
		in.Body, in.Size = dl, dl.Size
		if in.FileName == "" {
			in.FileName = urlFileName(src.URL)
		}
	} else {
		in.Body, in.Size = bytes.NewReader(src.Data), int64(len(src.Data))
	}
	if src.ImageOnly {
		in.ExpectKind = mediadomain.KindImage
	}
	up, err := a.Svc.Upload(ctx, in)
	if err != nil {
		return mcpapp.Reference{}, err
	}
	return mcpapp.Reference{ID: up.ID, Kind: string(up.Kind), MIME: up.MIME, SizeBytes: up.SizeBytes,
		Width: up.Width, Height: up.Height, DurationMs: up.DurationMs}, nil
}

// downloadError 把下载失败翻成用户看得懂的业务错误。出网策略拦下的说清「换一个公网地址」，
// 其余（连不上、对方 4xx/5xx、超时）说清是**那个地址**的问题 —— 不回显底层错误里的内网细节。
func downloadError(err error) error {
	switch {
	case errors.Is(err, providertransport.ErrPrivateNet), errors.Is(err, providertransport.ErrURLScheme),
		errors.Is(err, providertransport.ErrURLHost):
		return httpx.Errf(errcode.InvalidArgument,
			"这个地址不允许下载（只收公网可访问的 http/https 地址，内网与本机地址一律拒绝）："+err.Error())
	case errors.Is(err, providertransport.ErrTooLarge):
		return httpx.Errf(errcode.PayloadTooLarge,
			fmt.Sprintf("文件超过上限 %d MB", mcpUploadMaxBytes>>20))
	case errors.Is(err, context.DeadlineExceeded):
		return httpx.Errf(errcode.InvalidArgument, "下载超时：这个地址太慢或文件太大，请换一个地址")
	}
	return httpx.Errf(errcode.InvalidArgument, "从这个地址下载失败，请确认它公网可访问且直接返回文件内容")
}

func urlFileName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "reference"
	}
	if b := path.Base(u.Path); b != "" && b != "/" && b != "." {
		return b
	}
	return "reference"
}

// ---------------------------------------------------------------- 写画布

// MCPCanvasWriter 写画布：画布域 ApplyOps（canvas_op_log + canvas.ops_applied 事件，MCP-F-60），
// Actor{Kind:mcp, UserID, GrantID, ClientName}。所有权由画布域按 OwnedByActor 判，不符翻成 mcpapp.ErrNodeNotOwned。
type MCPCanvasWriter struct{ Svc *canvasapp.Service }

func (a MCPCanvasWriter) Apply(ctx context.Context, p mcpapp.Principal, in mcpapp.CanvasApplyInput) (
	mcpapp.CanvasApplyResult, error) {
	var ops []canvasdomain.Op
	if err := json.Unmarshal(in.Ops, &ops); err != nil {
		return mcpapp.CanvasApplyResult{}, httpx.Errf(errcode.InvalidArgument, "ops 格式不对："+err.Error())
	}
	res, err := a.Svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{
		WorkspaceID: p.WorkspaceID, ProjectID: in.ProjectID, BatchID: in.BatchID, Ops: ops,
		Actor: canvasdomain.Actor{Kind: canvasdomain.ActorMCP, UserID: p.UserID, GrantID: p.GrantID,
			ClientName: p.ClientName},
	})
	if errors.Is(err, canvasdomain.ErrNotOwned) {
		return mcpapp.CanvasApplyResult{}, fmt.Errorf("%w：%s", mcpapp.ErrNodeNotOwned, notOwnedNode(err))
	}
	if err != nil {
		return mcpapp.CanvasApplyResult{}, err
	}
	return mcpapp.CanvasApplyResult{NodeIDs: res.NodeIDs, FromVersion: res.FromVersion, ToVersion: res.ToVersion,
		Replayed: res.Replayed}, nil
}

// notOwnedNode 取出画布域错误里点名的节点（「…: a_1」），取不到就给原文。
func notOwnedNode(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return "节点 " + s[i+2:]
	}
	return s
}

// ---------------------------------------------------------------- 写文档

// MCPDocWriter 写项目文档（canvas 域 project_document）：version=0 新建，否则按路径找到后乐观锁更新；
// 来源记 mcp（DocumentActor.McpGrantID，00053）。冲突原样返回 canvas 的 40902（带「是谁改的」）。
type MCPDocWriter struct{ Svc *canvasapp.Service }

func (a MCPDocWriter) Put(ctx context.Context, p mcpapp.Principal, projectID int64, docPath, content string,
	version int) (mcpapp.Document, bool, error) {
	actor := canvasapp.DocumentActor{UserID: p.UserID, McpGrantID: p.GrantID}
	if version == 0 {
		d, err := a.Svc.CreateDocument(ctx, p.WorkspaceID, projectID, actor, p.Role,
			canvasapp.CreateDocumentInput{Path: docPath, Content: content})
		if err != nil {
			return mcpapp.Document{}, false, err
		}
		return mcpDocument(d.ID, d.Path, d.Version, d.Source, len(d.Content), d.UpdatedAt, d.EditorName), true, nil
	}
	cur, err := a.Svc.DocumentByPath(ctx, p.WorkspaceID, projectID, p.Role, docPath)
	if err != nil {
		return mcpapp.Document{}, false, err
	}
	d, err := a.Svc.UpdateDocument(ctx, p.WorkspaceID, projectID, cur.ID, actor, p.Role, version, content)
	if err != nil {
		return mcpapp.Document{}, false, err
	}
	return mcpDocument(d.ID, d.Path, d.Version, d.Source, len(d.Content), d.UpdatedAt, d.EditorName), false, nil
}

func mcpDocument(id int64, p string, version int, source string, bytes int, updated time.Time,
	editor string) mcpapp.Document {
	return mcpapp.Document{DocumentSummary: mcpapp.DocumentSummary{ID: id, Path: p, Version: version, Source: source,
		Bytes: bytes, UpdatedAt: updated}, EditorName: editor}
}

// ---------------------------------------------------------------- 建项目

// MCPProjectCreator 按幂等键建项目（canvas CreateWithIdemKey）：同键返回已有那个。key 已由工具层按授权加前缀。
type MCPProjectCreator struct{ Svc *canvasapp.Service }

func (a MCPProjectCreator) Create(ctx context.Context, p mcpapp.Principal, name, key string) (
	mcpapp.ProjectSummary, string, bool, error) {
	pr, replayed, err := a.Svc.CreateWithIdemKey(ctx, p.WorkspaceID, p.UserID, p.Role,
		canvasapp.CreateInput{Name: name}, key)
	if err != nil {
		return mcpapp.ProjectSummary{}, "", false, err
	}
	return projectSummary(pr), pr.Status, replayed, nil
}

var (
	_ mcpapp.TaskControl    = MCPTaskControl{}
	_ mcpapp.MediaUploader  = MCPUploader{}
	_ mcpapp.CanvasWriter   = MCPCanvasWriter{}
	_ mcpapp.DocumentWriter = MCPDocWriter{}
	_ mcpapp.ProjectCreator = MCPProjectCreator{}
)
