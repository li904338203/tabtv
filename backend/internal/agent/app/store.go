package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// RunnerStore 是轮次运行器用到的数据访问（agent/repo 的一个投影）。
//
// 抽成接口的唯一理由是单测：Runner 的分支（规划态下发什么、调用上限、停止、挂起）
// 要能用内存假实现跑；扣费 / 租约 / 重投的正确性仍由 repo/integration 的真库测试钉住。
// ⚠️ 不要往这里加「业务判断」：它只是 repo 方法的转发，事务边界也只在 repoStore 里。
type RunnerStore interface {
	GetSession(ctx context.Context, id int64) (repo.Session, error)
	GetTurn(ctx context.Context, id int64) (repo.Turn, error)
	ClaimLease(ctx context.Context, sessionID int64, runnerID string, ttl time.Duration) (bool, error)
	RenewLease(ctx context.Context, sessionID int64, runnerID string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, sessionID int64, runnerID string) (bool, error)
	SetTurnQuote(ctx context.Context, turnID int64, modelCode string, credits int) (bool, error)
	MarkTurnRunning(ctx context.Context, turnID int64) (bool, error)
	FinishTurn(ctx context.Context, turnID int64, status domain.TurnStatus,
		callCount, validOutputs int, errCode, errMsg string) (bool, error)
	TransitionState(ctx context.Context, sessionID int64, from, to domain.State) (bool, error)
	AppendMessage(ctx context.Context, m repo.Message) (repo.Message, error)
	RecentMessages(ctx context.Context, sessionID int64, limit int) ([]repo.Message, error)
	InsertStep(ctx context.Context, s repo.Step) (repo.Step, error)
	FinishStep(ctx context.Context, id int64, status string, display json.RawMessage, errMsg string) (bool, error)
	SetStepWaiting(ctx context.Context, id, generationID int64) (bool, error)
	HasWaitingSteps(ctx context.Context, sessionID int64) (bool, error)
	// TagStep 记步骤的计划项键与错误码（MKT-T-22）；空串 = 不动。
	TagStep(ctx context.Context, id int64, planItemKey, errorCode string) error
	UpdateSummary(ctx context.Context, sessionID int64, summary string) error
	SkillByCode(ctx context.Context, code string) (repo.Skill, error)
	// AnsweredQuestionCards 会话里已提交的问答卡（最新的在前）：视频规格三问按固定题目 id 从这里取回答。
	// ⚠️ 必需方法而不是「可选接口 + 类型断言」：后者在包装层漏转发时不报编译错误，回答永远读不到，
	// 用户答完规格还被要求再确认（集成测试 TestPlanningTurn_智能规格先确认再提交 抓到过）。
	AnsweredQuestionCards(ctx context.Context, sessionID int64, limit int) ([]repo.Card, error)
}

// NewRepoStore 把 agent/repo 接成 RunnerStore。
func NewRepoStore(r *repo.Repo) RunnerStore { return repoStore{r} }

type repoStore struct{ r *repo.Repo }

func (s repoStore) GetSession(ctx context.Context, id int64) (repo.Session, error) {
	return s.r.GetByID(ctx, id)
}

func (s repoStore) GetTurn(ctx context.Context, id int64) (repo.Turn, error) {
	return s.r.GetTurn(ctx, id)
}

func (s repoStore) AnsweredQuestionCards(ctx context.Context, sessionID int64, limit int) ([]repo.Card, error) {
	return s.r.AnsweredQuestionCards(ctx, sessionID, limit)
}

func (s repoStore) ClaimLease(ctx context.Context, id int64, runner string, ttl time.Duration) (bool, error) {
	return s.r.ClaimLease(ctx, id, runner, ttl)
}

func (s repoStore) RenewLease(ctx context.Context, id int64, runner string, ttl time.Duration) (bool, error) {
	return s.r.RenewLease(ctx, id, runner, ttl)
}

func (s repoStore) ReleaseLease(ctx context.Context, id int64, runner string) (bool, error) {
	return s.r.ReleaseLease(ctx, id, runner)
}

func (s repoStore) SetTurnQuote(ctx context.Context, id int64, model string, credits int) (bool, error) {
	return s.r.SetTurnQuote(ctx, id, model, credits)
}

func (s repoStore) MarkTurnRunning(ctx context.Context, id int64) (bool, error) {
	return s.r.MarkTurnRunning(ctx, id)
}

func (s repoStore) FinishTurn(ctx context.Context, id int64, status domain.TurnStatus,
	calls, valid int, code, msg string) (bool, error) {
	return s.r.FinishTurn(ctx, id, status, calls, valid, code, msg)
}

func (s repoStore) TransitionState(ctx context.Context, id int64, from, to domain.State) (bool, error) {
	tx, err := s.r.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ok, err := s.r.TransitionState(ctx, tx, id, from, to)
	if err != nil {
		return false, err
	}
	return ok, tx.Commit(ctx)
}

// AppendMessage 单条追加：自己开一个短事务（Append 依赖会话行锁分配 seq）。
func (s repoStore) AppendMessage(ctx context.Context, m repo.Message) (repo.Message, error) {
	tx, err := s.r.Begin(ctx)
	if err != nil {
		return repo.Message{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := s.r.Append(ctx, tx, m)
	if err != nil {
		return repo.Message{}, err
	}
	return out, tx.Commit(ctx)
}

func (s repoStore) RecentMessages(ctx context.Context, sessionID int64, limit int) ([]repo.Message, error) {
	return s.r.ListRecent(ctx, sessionID, limit)
}

func (s repoStore) InsertStep(ctx context.Context, st repo.Step) (repo.Step, error) {
	tx, err := s.r.Begin(ctx)
	if err != nil {
		return repo.Step{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := s.r.InsertStep(ctx, tx, st)
	if err != nil {
		return repo.Step{}, err
	}
	return out, tx.Commit(ctx)
}

func (s repoStore) FinishStep(ctx context.Context, id int64, status string, display json.RawMessage,
	errMsg string) (bool, error) {
	tx, err := s.r.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ok, err := s.r.FinishStep(ctx, tx, id, status, display, errMsg)
	if err != nil {
		return false, err
	}
	return ok, tx.Commit(ctx)
}

func (s repoStore) SetStepWaiting(ctx context.Context, id, generationID int64) (bool, error) {
	tx, err := s.r.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ok, err := s.r.SetStepWaiting(ctx, tx, id, generationID)
	if err != nil {
		return false, err
	}
	return ok, tx.Commit(ctx)
}

func (s repoStore) HasWaitingSteps(ctx context.Context, sessionID int64) (bool, error) {
	steps, err := s.r.WaitingBySession(ctx, sessionID)
	return len(steps) > 0, err
}

func (s repoStore) TagStep(ctx context.Context, id int64, planItemKey, errorCode string) error {
	return s.r.TagStep(ctx, s.r.Pool(), id, planItemKey, errorCode)
}

func (s repoStore) UpdateSummary(ctx context.Context, id int64, summary string) error {
	return s.r.UpdateSummary(ctx, id, summary)
}

func (s repoStore) SkillByCode(ctx context.Context, code string) (repo.Skill, error) {
	return s.r.GetSkillByCode(ctx, code)
}
