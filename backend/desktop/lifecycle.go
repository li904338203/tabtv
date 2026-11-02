package desktop

import (
	"context"
	"encoding/json"

	"github.com/li904338203/tabtv/backend/internal/platform/buildinfo"
	"github.com/li904338203/tabtv/backend/internal/platform/settings"
)

// ActiveWork 是还没结束的后台工作（TT-T-30）：关窗口前据此决定要不要询问。
//
// ⚠️ 只数 queued / running（整图运行与 Agent 回合的「排队」叫 pending）：成功、失败、取消的已经落定，
// 关掉也不会丢什么。不新增表 —— 各域的状态列就是事实。
type ActiveWork struct {
	Generations int `json:"generations"`
	Workflows   int `json:"workflows"`
	Renders     int `json:"renders"`
	Exports     int `json:"exports"`
	AgentTurns  int `json:"agentTurns"`
}

// Total 是总数。
func (w ActiveWork) Total() int {
	return w.Generations + w.Workflows + w.Renders + w.Exports + w.AgentTurns
}

const activeWorkSQL = `select
	(select count(*) from generation where status in ('queued','running')),
	(select count(*) from workflow_run where status in ('pending','running')),
	(select count(*) from render_job where status in ('queued','running')),
	(select count(*) from export_task where status in ('queued','running')),
	(select count(*) from agent_turn where status in ('pending','running'))`

// ActiveWork 查询当前还在排队或运行的工作。
func (r *Runtime) ActiveWork(ctx context.Context) (ActiveWork, error) {
	var w ActiveWork
	err := r.pool.QueryRow(ctx, activeWorkSQL).Scan(&w.Generations, &w.Workflows, &w.Renders, &w.Exports, &w.AgentTurns)
	return w, err
}

// CloseAlwaysMinimize 读「不再询问，总是最小化」。
func (r *Runtime) CloseAlwaysMinimize() bool { return r.settings.Bool(settings.AppCloseMinimize) }

// SetCloseAlwaysMinimize 写「不再询问，总是最小化」（关闭对话框里勾选时调用）。
func (r *Runtime) SetCloseAlwaysMinimize(ctx context.Context, on bool) error {
	raw, _ := json.Marshal(on)
	_, err := r.settings.Set(ctx, map[string]json.RawMessage{settings.AppCloseMinimize: raw})
	return err
}

// UpdateAutoCheck 读「自动检查更新」（TT-T-31）。
func (r *Runtime) UpdateAutoCheck() bool { return r.settings.Bool(settings.UpdateAutoCheck) }

// BuildInfo 给宿主（自动更新、关于页）用：宿主在 backend 模块之外，不能直接引用 internal 包。
func BuildInfo() buildinfo.Info { return buildinfo.Get() }
