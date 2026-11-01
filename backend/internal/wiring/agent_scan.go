package wiring

// 画布 Agent 扫描循环（AGT-25）的装配：任务批量查询端口 + 扫描器本身。
// 单独成文件，与付费工具（AGT-24）在 agent.go 里的 Generator 适配器互不干扰。

import (
	"context"
	"log/slog"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	workflowrepo "github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

// AgentTaskStatuses 把生成域的批量快照接成 agentapp.TaskStatuses。
//
// Refunded = 账本上有退款流水，或者根本没扣过（0 积分 / 免费）—— 两种情况额度都可以归还。
type AgentTaskStatuses struct{ Repo *genrepo.Repo }

func (a AgentTaskStatuses) Statuses(ctx context.Context, ws int64, ids []int64) (map[int64]agentapp.TaskResult, error) {
	snaps, err := a.Repo.TaskSnapshots(ctx, ws, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]agentapp.TaskResult, len(snaps))
	for _, s := range snaps {
		r := agentapp.TaskResult{ID: s.ID, Found: true, Status: s.Status, Terminal: gendomain.IsTerminal(s.Status),
			ErrorCode: s.ErrorCode, ErrorMessage: s.ErrorMessage, Refunded: s.Refunded || !s.Consumed}
		for _, o := range s.Outputs {
			r.Outputs = append(r.Outputs, agentapp.TaskOutput{Idx: o.Idx, Type: o.OutputType,
				MediaID: o.MediaObjectID, Text: o.TextContent})
		}
		out[s.ID] = r
	}
	return out, nil
}

// AgentScannerInput 是 Worker 装配扫描循环要的东西。
type AgentScannerInput struct {
	AgentRepo  *agentrepo.Repo
	GenRepo    *genrepo.Repo
	Billing    *billingrepo.Repo
	Bus        BusPublisher
	Compensate time.Duration
	NextID     func() int64
	Logger     *slog.Logger
	MaxCalls   int
	CardExpire time.Duration
	// 多段工具（AGT-50）挂的业务对象：给齐了才按引用唤醒 / 结算（缺一个就不接，对应条目不处理）。
	ScriptRepo   *scriptrepo.Repo
	WorkflowRepo *workflowrepo.Repo
	TimelineRepo *timelinerepo.Repo
}

// NewAgentScanner 装出 Worker 侧的扫描循环。退款走与轮次运行器同一个 AgentLedger。
func NewAgentScanner(in AgentScannerInput) *agentapp.Scanner {
	var events agentapp.Events
	if in.Bus != nil {
		events = AgentEvents{in.Bus}
	}
	var refs agentapp.RefStatuses
	if in.ScriptRepo != nil && in.WorkflowRepo != nil && in.TimelineRepo != nil {
		refs = AgentRefStatuses{Gen: in.GenRepo, Script: in.ScriptRepo, Workflow: in.WorkflowRepo,
			Timeline: in.TimelineRepo, Billing: in.Billing}
	}
	return agentapp.NewScanner(agentapp.ScannerDeps{
		Repo:   in.AgentRepo,
		Tasks:  AgentTaskStatuses{in.GenRepo},
		Refs:   refs,
		Ledger: AgentLedger{Repo: in.Billing, Compensate: in.Compensate},
		Events: events,
		NextID: in.NextID,
		Logger: in.Logger,
	}, agentapp.ScannerConfig{MaxCalls: in.MaxCalls, CardExpire: in.CardExpire})
}

var _ agentapp.TaskStatuses = AgentTaskStatuses{}
