// Package tools 装配 MCP 的业务工具。
//
// ⚠️ NewRegistry 是**唯一**的注册表构造函数：cmd/api（运行时）与 tools/mcpschema（快照）
// 都调它（MT-8）。各自另建一份的话，快照与线上会悄悄分叉 —— 文档里有、线上没有。
//
// 工具通过 app 端口调用各域服务，不 import 任何域的 repo（开发 01 §2）。
package tools

import (
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
)

// Deps 是工具需要的各域端口（实现在 internal/wiring/mcp_read.go）。
//
// ⚠️ 快照生成器传的是零值 Deps：工具的**注册**（名字、method、schema）不能依赖端口是否为 nil，
// 只有 handler 执行时才用到端口。
type Deps struct {
	Account   app.Account
	Projects  app.Projects
	Canvas    app.Canvas
	Documents app.Documents
	Catalog   app.Catalog
	Tasks     app.Tasks
	Media     app.Media
	// Quotes 是报价与 quoteId 签发（MCPT-05）。
	Quotes app.QuoteService
	// CanvasGen / Confirm 是 generate 的画布生成编排与花钱确认服务（MCPT-10）。
	CanvasGen app.CanvasGeneration
	Confirm   *app.Confirm
	// 写工具（MCPT-11）：任务取消、参考文件上传、写画布、写文档、建项目。
	TaskControl    app.TaskControl
	Uploader       app.MediaUploader
	CanvasWriter   app.CanvasWriter
	DocWriter      app.DocumentWriter
	ProjectCreator app.ProjectCreator
	// 3D 导演台（DRT-29）：建导演台节点与场景、AI 搭景（画布工具集，不花积分）。
	Directors app.Directors
	// 智能剪辑（SCT-41）：建节点并连素材、只读详情、免费的 AI 剪辑、导出（画布工具集，默认开启）。
	Smartcuts app.Smartcuts
	// 多段工具集（MCPT-16）：脚本线 / 整图 / 时间线（默认关闭的工具集），与提示词来源（站内技能）。
	Scripts   app.Scripts
	Workflows app.Workflows
	Timelines app.Timelines
	Skills    app.Skills
	// 委托站内 Agent（MCPT-17）：会话端口；WebBase 是用户端地址（SEEAITV_WEB_PUBLIC_URL，会话链接用）。
	// AgentDisabled=true（站内 Agent 关闭）时 agent 工具不注册 —— 零值注册，快照里有它。
	Agent         app.AgentSessions
	WebBase       string
	AgentDisabled bool
	// SmartcutAIDisabled=true（SEEAITV_SMARTCUT_AI_ENABLED 关）时 smartcut_plan 不注册 —— 零值注册，快照里有它。
	SmartcutAIDisabled bool
	// WaitMax 是 tasks.wait 的上限（SEEAITV_MCP_WAIT_MAX，0 = 默认 50 秒）；Clock 为 nil 时用系统时钟（单测注入）。
	WaitMax time.Duration
	Clock   app.Clock
}

// NewRegistry 构造包含全部业务工具的注册表。
func NewRegistry(d Deps) *app.Registry {
	r := app.NewRegistry()
	// M1 只读工具（MCPT-04）。
	r.Register(whoamiTool(d))
	r.Register(projectsTool(d))
	r.Register(canvasReadTool(d))
	r.Register(documentsTool(d))
	r.Register(modelsTool(d))
	r.Register(imageToolsTool(d))
	r.Register(tasksTool(d))
	r.Register(mediaTool(d))
	// 报价（MCPT-05）：只读，签发 quoteId 供 M2 的花钱确认。
	r.Register(quoteTool(d))
	// 花钱（MCPT-10）：Paid + Write，只读端点看不到（domain.Visible 自动挡）。
	r.Register(generateTool(d))
	// 写工具（MCPT-11）：tasks.wait / cancel、media.upload、documents.put、projects.create 是既有工具上的 method。
	r.Register(canvasApplyTool(d))
	// 3D 导演台（DRT-29）：与 canvas_apply 同形的单方法写工具，只读端点看不到。
	r.Register(directorCreateTool(d))
	r.Register(directorBuildTool(d))
	// 智能剪辑（SCT-41，SC-F-165）：与导演台同处默认开启的画布工具集。smartcut_get 只读（只读凭据看得到），
	// 其余写画布 / 时间线；smartcut_export 走同一条导出路径（timeline 工具集默认关闭，客户端默认用不了 timeline.render）。
	r.Register(smartcutCreateTool(d))
	r.Register(smartcutGetTool(d))
	if !d.SmartcutAIDisabled {
		r.Register(smartcutPlanTool(d))
	}
	r.Register(smartcutExportTool(d))
	// 多段工具集（MCPT-16）：默认关闭的 script / workflow / timeline；付费 method 走确认服务（按报价金额，
	// 0 积分不确认）。资源与提示词（MCP-F-36 / 37）。
	r.Register(scriptTool(d))
	r.Register(workflowTool(d))
	r.Register(timelineTool(d))
	// 委托站内 Agent（MCPT-17）：默认关闭的 agent 工具集；站内 Agent 关闭时整个工具不出现（列表、快照片段都没有）。
	if !d.AgentDisabled {
		r.Register(agentTool(d))
	}
	registerResources(r, d)
	r.SetPrompts(skillPrompts{d})
	return r
}

// unwired 是端口未注入时的错误（零值 Deps 只用于快照，线上必须全部注入）。
func unwired(port string) error { return errUnwired{port} }

type errUnwired struct{ port string }

func (e errUnwired) Error() string { return "MCP 端口 " + e.port + " 未接入" }
