package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// workflow（MCPT-16，MCP-F-34）：整图预览 / 运行 / 查询 / 取消。默认关闭的 workflow 工具集。
//
// ⚠️ plan **只读、不扣费、不建运行**（NEW-01），所以它是 view、在 /mcp/readonly 上可见；run 才花钱。
// ⚠️ 计划有问题（环、悬空边、模型不可用……）时 run 一分钱不扣，把问题与涉及的节点原样回给模型，不进确认。
// ⚠️ 运行没有运行级幂等键：同一确认的重试靠 quote 的 CAS + 记在 quote 上的运行 ID 回放，不会建第二个运行。
// ⚠️ cancel 走既有的 TaskControl（kind=workflow，与 tasks.cancel 同一条路）：先标运行再取消子任务，
// 子任务的费用由生成域按原批次退回、只退一次 —— 这里没有任何与金额有关的参数。

const maxTargetNodes = 100

type workflowIn struct {
	ProjectID     string   `json:"projectId,omitempty" jsonschema:"plan / run：项目 ID"`
	TargetNodeIDs []string `json:"targetNodeIds,omitempty" jsonschema:"plan / run：只产出这些节点（含其全部上游）；不给：整图"`
	Force         bool     `json:"force,omitempty" jsonschema:"plan / run：不复用已有产出，全部重跑（会多扣费）"`
	RunID         string   `json:"runId,omitempty" jsonschema:"get / cancel：运行 ID"`
	Reason        string   `json:"reason,omitempty" jsonschema:"cancel：取消原因（可选）"`
	// 确认（run）
	ConfirmQuoteID string `json:"confirmQuoteId,omitempty" jsonschema:"run 的确认凭证：上一次调用返回的 quoteId"`
	ConfirmCredits *int   `json:"confirmCredits,omitempty" jsonschema:"回显报价金额，必须等于该 quoteId 的金额；与 confirmQuoteId 一起给"`
}

type workflowIssueOut struct {
	Message string   `json:"message"`
	Nodes   []string `json:"nodes,omitempty" jsonschema:"涉及的画布节点"`
}

type workflowBlockedOut struct {
	NodeID string `json:"nodeId"`
	Reason string `json:"reason"`
}

type workflowPlanOut struct {
	Executable       bool                 `json:"executable"`
	Summary          string               `json:"summary"`
	Issues           []workflowIssueOut   `json:"issues,omitempty"`
	Blocked          []workflowBlockedOut `json:"blocked,omitempty" jsonschema:"想跑跑不了的节点（与「不需要跑」的 skip 不是一回事）"`
	RunCount         int                  `json:"runCount"`
	ReuseCount       int                  `json:"reuseCount" jsonschema:"设置没变且已有产出、复用而不扣费的节点数"`
	SkipCount        int                  `json:"skipCount"`
	TotalCredits     int                  `json:"totalCredits"`
	EstimatedSeconds int                  `json:"estimatedSeconds" jsonschema:"按关键路径估算的耗时"`
}

type workflowNodeOut struct {
	NodeID       string `json:"nodeId"`
	Status       string `json:"status" jsonschema:"pending / running / success / failed / skipped / blocked / cancelled"`
	GenerationID string `json:"generationId,omitempty"`
	Credits      int    `json:"credits"`
	Reused       bool   `json:"reused,omitempty"`
	Error        string `json:"error,omitempty"`
}

type workflowRunOut struct {
	ID           string            `json:"id"`
	ProjectID    string            `json:"projectId,omitempty"`
	Status       string            `json:"status" jsonschema:"pending / running / success / partial_success / failed / cancelled"`
	TotalCredits int               `json:"totalCredits"`
	Nodes        []workflowNodeOut `json:"nodes,omitempty"`
}

type workflowOut struct {
	Status            string                `json:"status" jsonschema:"plan：executable / not_executable；run：已受理的运行状态 / needs_confirmation / declined / not_executable；get：运行状态；cancel：取消后的运行状态"`
	RunID             string                `json:"runId,omitempty"`
	Credits           int                   `json:"credits" jsonschema:"run：本次确认的积分（按计划报价；实扣以账本为准，失败的子任务会退回）"`
	Replayed          bool                  `json:"replayed"`
	Cancelled         *bool                 `json:"cancelled,omitempty" jsonschema:"cancel：true 由本次取消；false 运行早已结束（不是错误）"`
	NeedsConfirmation *needsConfirmationOut `json:"needsConfirmation,omitempty"`
	Plan              *workflowPlanOut      `json:"plan,omitempty"`
	Run               *workflowRunOut       `json:"run,omitempty"`
	Message           string                `json:"message"`

	elicit *app.ElicitRequest
}

func (o workflowOut) ElicitRequest() *app.ElicitRequest { return o.elicit }

const workflowDescription = "按依赖关系运行整张画布（或只跑到 targetNodeIds 为止的那部分）。" +
	"plan：运行前预览（免费、只读、可反复调用）：环 / 悬空边 / 模型不可用等问题与涉及的节点、每个节点是跑 / 复用 / 跳过 / 被卡、" +
	"费用与按关键路径估算的耗时；run：按 plan 的费用运行（付费，设置没变且已有产出的节点复用不扣费；计划有问题时不扣费并返回问题）；" +
	"get：运行与逐节点状态；cancel：取消运行（在跑的子任务一并取消，费用由原任务按原路退回）。" + paidDescTail

func workflowTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:        "workflow",
		Toolset:     domain.ToolsetWorkflow,
		Description: workflowDescription,
		Methods: []app.Method{
			{Name: "plan", Action: collab.ActionView},
			{Name: "run", Action: collab.ActionGenerate, Paid: true, Write: true},
			{Name: "get", Action: collab.ActionView},
			// 中止在跑的付费任务、已花在上游的拿不回来：如实标 Destructive（同 tasks.cancel）。
			{Name: "cancel", Action: collab.ActionGenerate, Write: true, Destructive: true},
		},
	}, func(ctx context.Context, method string, in workflowIn) (workflowOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return workflowOut{}, "", err
		}
		if d.Workflows == nil {
			return workflowOut{}, "", unwired("Workflows")
		}
		switch method {
		case "plan":
			req, err := workflowPlanInput(in)
			if err != nil {
				return workflowOut{}, "", err
			}
			plan, err := d.Workflows.Plan(ctx, p, req)
			if err != nil {
				return workflowOut{}, "", err
			}
			out := workflowOut{Status: planStatus(plan), Plan: planOut(plan),
				Message: "这是预览：没有建运行、没有扣费。确认后用 workflow.run（参数相同）运行。"}
			if !plan.Executable {
				out.Message = "计划不可执行：按 issues / blocked 里的节点修正后再运行。这是预览，没有扣费。"
			}
			return out, plan.Summary, nil
		case "run":
			return workflowRun(ctx, d, p, in)
		case "get":
			id, err := parseID("runId", in.RunID)
			if err != nil {
				return workflowOut{}, "", err
			}
			r, err := d.Workflows.Run(ctx, p, id)
			if err != nil {
				return workflowOut{}, "", err
			}
			return workflowOut{Status: r.Status, RunID: idStr(r.ID), Run: runOut(r),
				Message: "整图运行的当前状态。blocked 是因上游失败 / 跳过而跑不了的节点，skipped 是不需要跑的。"}, "运行 " + r.Status, nil
		case "cancel":
			return workflowCancel(ctx, d, p, in)
		}
		return workflowOut{}, "", httpx.Errf(errcode.InvalidArgument, "未知 method："+method)
	})
}

func workflowPlanInput(in workflowIn) (app.WorkflowPlanInput, error) {
	pid, err := parseID("projectId", in.ProjectID)
	if err != nil {
		return app.WorkflowPlanInput{}, err
	}
	if len(in.TargetNodeIDs) > maxTargetNodes {
		return app.WorkflowPlanInput{}, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("targetNodeIds 最多 %d 个", maxTargetNodes))
	}
	targets := make([]string, 0, len(in.TargetNodeIDs))
	for _, id := range in.TargetNodeIDs {
		if id = strings.TrimSpace(id); id != "" {
			targets = append(targets, id)
		}
	}
	return app.WorkflowPlanInput{ProjectID: pid, TargetNodeIDs: targets, Force: in.Force}, nil
}

func workflowRun(ctx context.Context, d Deps, p app.Principal, in workflowIn) (workflowOut, string, error) {
	req, err := workflowPlanInput(in)
	if err != nil {
		return workflowOut{}, "", err
	}
	// 有确认凭证的重调先过确认（可能是重放），不先跑预览；首次调用先预览：有问题就一分钱不扣、直接回问题。
	var first *app.WorkflowPlan
	if in.ConfirmQuoteID == "" && in.ConfirmCredits == nil && app.ElicitationFrom(ctx).Answer == nil {
		plan, err := d.Workflows.Plan(ctx, p, req)
		if err != nil {
			return workflowOut{}, "", err
		}
		if !plan.Executable {
			return workflowOut{Status: "not_executable", Plan: planOut(plan),
				Message: "计划不可执行，没有扣费。按 issues / blocked 里的节点修正后再运行。"}, plan.Summary, nil
		}
		first = &plan
	}
	fp := paidFingerprint("workflow.run", map[string]any{"projectId": idStr(req.ProjectID),
		"targetNodeIds": req.TargetNodeIDs, "force": req.Force})
	var plan app.WorkflowPlan
	g, err := gatePaid(ctx, d, p, paidAsk{Tool: "workflow.run", Fingerprint: fp,
		ConfirmQuoteID: in.ConfirmQuoteID, ConfirmCredits: in.ConfirmCredits,
		Price: func(ctx context.Context) (app.Priced, error) {
			if first != nil {
				plan, first = *first, nil
			} else {
				pl, err := d.Workflows.Plan(ctx, p, req)
				if err != nil {
					return app.Priced{}, err
				}
				plan = pl
			}
			if !plan.Executable {
				return app.Priced{}, httpx.Errf(errcode.InvalidArgument, "计划已不可执行（"+plan.Summary+
					"），没有扣费：用 workflow.plan 查看问题与涉及的节点").WithDetails(map[string]any{"reason": "not_executable"})
			}
			return app.Priced{Credits: plan.TotalCredits, Detail: map[string]any{"projectId": idStr(req.ProjectID),
				"runCount": plan.RunCount, "reuseCount": plan.ReuseCount, "targets": len(req.TargetNodeIDs)},
				Summary: fmt.Sprintf("运行整图：%s", plan.Summary)}, nil
		}})
	if err != nil {
		return workflowOut{}, "", err
	}
	base := workflowOut{}
	switch g.Kind {
	case gateConfirm:
		if g.Pending.Elicit != nil {
			return workflowOut{elicit: g.Pending.Elicit}, "", nil
		}
		nc, msg, summary := pendingOut(g.Pending, "workflow.run")
		base.Status, base.NeedsConfirmation, base.Message, base.Plan = "needs_confirmation", nc, msg, planOut(plan)
		return base, summary, nil
	case gateDeclined:
		base.Status, base.Message = "declined", declinedMessage
		return base, "用户已拒绝，未扣费", nil
	case gateInProgress:
		base.Status, base.Credits, base.Replayed, base.Message = "running", g.Verified.Credits, true, inProgressMessage
		return base, "这个确认已受理，仍在处理中", nil
	case gateReplay:
		if err := replayedFailure(g.Result); err != nil {
			return workflowOut{}, "", err
		}
		var ref struct {
			RunID string `json:"runId"`
		}
		_ = json.Unmarshal(g.Result, &ref)
		id, err := parseID("runId", ref.RunID)
		if err != nil {
			return workflowOut{}, "", httpx.Wrap(errcode.Internal, err)
		}
		r, err := d.Workflows.Run(ctx, p, id)
		if err != nil {
			return workflowOut{}, "", err
		}
		return workflowOut{Status: r.Status, RunID: idStr(r.ID), Credits: g.Verified.Credits, Replayed: true,
			Run: runOut(r), Message: paidReplayMessage + "用 workflow.get 或 tasks 查看进度。"}, "这个确认已受理过，本次没有扣费", nil
	}
	r, err := d.Workflows.Start(ctx, p, req)
	if err != nil {
		recordPaid(ctx, d, p, g, failureOf(err))
		return workflowOut{}, "", err
	}
	recordPaid(ctx, d, p, g, map[string]string{"runId": idStr(r.ID)})
	out := workflowOut{Status: r.Status, RunID: idStr(r.ID), Credits: g.Verified.Credits, Run: runOut(r), Plan: planOut(plan),
		Message: "整图运行已受理（后台任务，下一批节点由服务端按依赖派发）。宿主超时不代表失败，不要重新提交；用 workflow.get 或 tasks（kind=workflow）查看。"}
	return out, fmt.Sprintf("已启动整图运行 %s（%d 积分）", out.RunID, g.Verified.Credits), nil
}

func workflowCancel(ctx context.Context, d Deps, p app.Principal, in workflowIn) (workflowOut, string, error) {
	id, err := parseID("runId", in.RunID)
	if err != nil {
		return workflowOut{}, "", err
	}
	if d.TaskControl == nil {
		return workflowOut{}, "", unwired("TaskControl")
	}
	reason := domain.CleanText(strings.TrimSpace(in.Reason), maxErrorRunes)
	res, err := d.TaskControl.Cancel(ctx, p, "workflow", id, reason)
	if err != nil {
		return workflowOut{}, "", err
	}
	r, err := d.Workflows.Run(ctx, p, id)
	if err != nil {
		return workflowOut{}, "", err
	}
	c := res.Cancelled
	out := workflowOut{Status: r.Status, RunID: idStr(r.ID), Cancelled: &c, Run: runOut(r)}
	if c {
		out.Message = "已取消整图运行：在跑的子任务一并取消，费用由原任务按原路退回（只退一次）。"
	} else {
		out.Message = "运行早已结束，没有改动任何东西（这不是错误）。"
	}
	return out, out.Message, nil
}

func planStatus(p app.WorkflowPlan) string {
	if p.Executable {
		return "executable"
	}
	return "not_executable"
}

func planOut(p app.WorkflowPlan) *workflowPlanOut {
	out := &workflowPlanOut{Executable: p.Executable, Summary: p.Summary, RunCount: p.RunCount, ReuseCount: p.ReuseCount,
		SkipCount: p.SkipCount, TotalCredits: p.TotalCredits, EstimatedSeconds: p.EstimatedSeconds}
	for _, is := range p.Issues {
		out.Issues = append(out.Issues, workflowIssueOut{Message: is.Message, Nodes: is.Nodes})
	}
	for _, b := range p.Blocked {
		out.Blocked = append(out.Blocked, workflowBlockedOut{NodeID: b.NodeID, Reason: b.Reason})
	}
	return out
}

func runOut(r app.WorkflowRunView) *workflowRunOut {
	out := &workflowRunOut{ID: idStr(r.ID), ProjectID: idStr(r.ProjectID), Status: r.Status, TotalCredits: r.TotalCredits}
	for _, n := range r.Nodes {
		out.Nodes = append(out.Nodes, workflowNodeOut{NodeID: n.NodeID, Status: n.Status,
			GenerationID: idStr(n.GenerationID), Credits: n.Credits, Reused: n.Reused,
			Error: domain.CleanText(n.Error, maxErrorRunes)})
	}
	return out
}
