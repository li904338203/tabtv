package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// run_workflow（AGT-50）：先 Plan（只读、不扣费）拿 totalCredits 与问题清单，再在额度内 Start。
//
// ⚠️ Start 没有运行级幂等键 —— **由步骤状态防重**：同一步骤的额度条目已挂着运行 ID 时，
// PaidCall 直接回那一次（Replayed），这里取那次运行的状态，**不再 Start**（租约过期重放同一步骤
// 不会建第二个整图运行）。
// ⚠️ 计划有问题就一分钱不扣，把问题与涉及的节点原样回给模型（NEW-01）。

// RunWorkflow 是 run_workflow。
type RunWorkflow struct{ d MultiDeps }

func (RunWorkflow) Name() domain.ToolName { return domain.ToolRunWorkflow }
func (RunWorkflow) Paid() bool            { return true }
func (RunWorkflow) Action() collab.Action { return collab.ActionGenerate }

func (RunWorkflow) Description() string {
	return "按依赖关系运行整张画布（或只跑到 targetNodeIds 为止的那部分）：设置没变且已有产出的节点复用、不扣费。" +
		"运行前服务端先预览计划，有环 / 缺输入 / 模型不可用时不扣费并返回涉及的节点。wait=true（默认）提交后暂停等运行结束。"
}

func (RunWorkflow) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"targetNodeIds":{"type":"array","items":{"type":"string"},"description":"只产出这些节点（含其全部上游）；不给=整图"},` +
		`"force":{"type":"boolean","description":"不复用已有产出，全部重跑（会多扣费）"},` +
		`"wait":{"type":"boolean","default":true}}}`)
}

type runWorkflowArgs struct {
	TargetNodeIDs []string `json:"targetNodeIds"`
	Force         bool     `json:"force"`
	Wait          *bool    `json:"wait"`
}

func (t RunWorkflow) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolRunWorkflow); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Workflows == nil || t.d.Budget == nil {
		return app.ToolResult{}, errors.New("整图服务未接入")
	}
	var a runWorkflowArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	targets := make([]string, 0, len(a.TargetNodeIDs))
	for _, id := range a.TargetNodeIDs {
		if id = strings.TrimSpace(id); id != "" {
			targets = append(targets, id)
		}
	}
	pr := app.WorkflowPlanRequest{WorkspaceID: rc.WorkspaceID, ProjectID: rc.ProjectID,
		MemberLevelID: rc.MemberLevelID, TargetNodeIDs: targets, Force: a.Force}
	plan, err := t.d.Workflows.Plan(ctx, pr)
	if err != nil {
		return app.ToolResult{}, fmt.Errorf("整图预览失败：%v", err)
	}
	if !plan.Executable {
		// 一分钱不扣：问题与涉及的节点原样回给模型，让它去改（或告诉用户）。
		content := map[string]any{"executable": false, "summary": plan.Summary, "issues": plan.Issues,
			"blocked": plan.Blocked, "message": "计划不可执行，没有扣费。按 issues / blocked 里的节点修正后再运行"}
		return app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{"executable": false,
			"summary": plan.Summary}), Outputs: 1}, nil
	}
	var run app.WorkflowRunView
	res, susp, err := t.d.Budget.PaidCallTool(ctx, rc, string(domain.ToolRunWorkflow), plan.TotalCredits,
		func(string) (app.PaidResult, error) {
			v, err := t.d.Workflows.Start(ctx, app.WorkflowStartRequest{WorkflowPlanRequest: pr,
				PayerAccountID: rc.PayerAccountID, UserID: rc.UserID})
			if err != nil {
				return app.PaidResult{}, err
			}
			run = v
			// 实扣要等子任务一个个受理（下一批由 API 的扫描循环派发），先按计划报价占额度，
			// 运行结束后扫描循环按账本结算。
			return app.PaidResult{Credits: -1, RefType: "workflow_run", RefID: v.ID}, nil
		})
	if susp != nil {
		return overBudgetResult(susp, plan.TotalCredits, nil), nil
	}
	if err != nil {
		return app.ToolResult{}, paidError("整图运行", plan.TotalCredits, err)
	}
	if res.Replayed {
		// 同一步骤重入：回那一次运行，不再 Start。
		if run, err = t.d.Workflows.Run(ctx, rc.WorkspaceID, res.RefID); err != nil {
			return app.ToolResult{}, err
		}
	}
	wait := wantWait(a.Wait)
	content := map[string]any{"runId": idS(run.ID), "status": run.Status, "summary": plan.Summary,
		"quoted": plan.TotalCredits, "runCount": plan.RunCount, "reuseCount": plan.ReuseCount,
		"estimatedSeconds": plan.EstimatedSeconds, "waiting": wait}
	if res.Replayed {
		content["replayed"] = true
	}
	if len(plan.Blocked) > 0 {
		content["blocked"] = plan.Blocked
	}
	out := app.ToolResult{Content: ptJSON(content), Display: ptJSON(map[string]any{"runId": idS(run.ID),
		"credits": plan.TotalCredits, "summary": plan.Summary}), Outputs: 1, WaitRef: true}
	if wait {
		out.Suspend = waitSuspension("等待整图运行结束")
	}
	return out, nil
}

var _ app.Tool = RunWorkflow{}
