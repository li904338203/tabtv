package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 本文件是 API 侧服务（AGT-26，agent/app/service.go）额外需要的读写。
// 与 AGT-21 的方法同一套约定：状态推进一律 CAS、返回「本次是否真的改了」。

// UpdateSessionMeta 改标题与会话级偏好。title=nil / prefs=nil 表示该项不改。
//
// ⚠️ prefs 整份替换（原样存，保留前端未知字段），不做 jsonb 合并：
// 合并会让「清掉某项偏好」无从表达。
func (r *Repo) UpdateSessionMeta(ctx context.Context, q Querier, id int64, title *string,
	prefs json.RawMessage) (Session, error) {
	var p any
	if len(prefs) > 0 {
		p = prefs
	}
	return scanSession(q.QueryRow(ctx, `
		update agent_session set title = coalesce($2, title), prefs = coalesce($3::jsonb, prefs),
			updated_at = now()
		where id = $1
		returning `+sessionCols, id, title, p))
}

// SetSkillsInTx 在事务里写已加载的技能（发消息时带 skillCode）。
func (r *Repo) SetSkillsInTx(ctx context.Context, q Querier, id int64, skillCodes json.RawMessage) error {
	_, err := q.Exec(ctx, `update agent_session set skill_codes = $2, updated_at = now() where id = $1`,
		id, jsonOr(skillCodes, "[]"))
	return err
}

// ClearPlanVersion 规划作废时清空 plan_doc_version（额度不动：已花掉的仍算在额度里）。
func (r *Repo) ClearPlanVersion(ctx context.Context, q Querier, id int64) error {
	_, err := q.Exec(ctx, `update agent_session set plan_doc_version = null, updated_at = now()
		where id = $1`, id)
	return err
}

// GetCardForUpdate 在事务里锁住卡片行并读出来（批准前再比一次合计用）。
func (r *Repo) GetCardForUpdate(ctx context.Context, tx pgx.Tx, sessionID, id int64) (Card, error) {
	return scanCard(tx.QueryRow(ctx, `select `+cardCols+` from agent_card
		where id = $1 and session_id = $2 for update`, id, sessionID))
}

// UpdateCardPayload 改写待处理卡片的内容（批准时价格变了：写入新报价，前端刷新后再确认）。
// 只改 pending 的；返回是否改到。
func (r *Repo) UpdateCardPayload(ctx context.Context, q Querier, id int64, payload json.RawMessage) (bool, error) {
	tag, err := q.Exec(ctx, `update agent_card set payload = $2 where id = $1 and status = 'pending'`,
		id, jsonOr(payload, "{}"))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CloseCard CAS pending → submitted / voided，**同时**记下提交内容与提交人。
//
// 「需要调整」要把修改意见留在卡片上、而卡片本身标作废 —— 先 SubmitCard 再 VoidCard
// 做不到（VoidCard 只从 pending 出），所以单独一条。
func (r *Repo) CloseCard(ctx context.Context, q Querier, id int64, status domain.CardStatus,
	submission json.RawMessage, userID int64) (bool, error) {
	if status != domain.CardSubmitted && status != domain.CardVoided {
		return false, errors.New("CloseCard 只接受 submitted / voided")
	}
	tag, err := q.Exec(ctx, `
		update agent_card set status = $2, submission = $3, submitted_by = $4, submitted_at = now()
		where id = $1 and status = 'pending'`, id, status, jsonOr(submission, "{}"), userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CardsByIDs 按 ID 批量取会话里的卡片（消息分页时带出卡片展示数据）。
func (r *Repo) CardsByIDs(ctx context.Context, sessionID int64, ids []int64) ([]Card, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `select `+cardCols+` from agent_card
		where session_id = $1 and id = any($2) order by created_at, id`, sessionID, ids)
	if err != nil {
		return nil, err
	}
	return collectCards(rows)
}

// StepsByTurns 列出若干轮次的步骤（开始时间顺序）。
func (r *Repo) StepsByTurns(ctx context.Context, sessionID int64, turnIDs []int64) ([]Step, error) {
	if len(turnIDs) == 0 {
		return nil, nil
	}
	return r.querySteps(ctx, `select `+stepCols+` from agent_step
		where session_id = $1 and turn_id = any($2) order by started_at, id`, sessionID, turnIDs)
}

// GenerationStepsBySession 列出会话里挂着生成任务、且步骤本身还没结束的步骤
// （停止时「列出可取消项」、取消会话时逐个取消）。任务是否已终态由调用方再问生成域。
func (r *Repo) GenerationStepsBySession(ctx context.Context, sessionID int64) ([]Step, error) {
	return r.querySteps(ctx, `select `+stepCols+` from agent_step
		where session_id = $1 and generation_id is not null and status in ('running', 'waiting')
		order by started_at, id`, sessionID)
}

// TurnsByIDs 批量取轮次（消息分页时带出每轮的状态与费用）。
func (r *Repo) TurnsByIDs(ctx context.Context, sessionID int64, ids []int64) ([]Turn, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `select `+turnCols+` from agent_turn
		where session_id = $1 and id = any($2) order by created_at, id`, sessionID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LatestTurn 取会话最近的一轮（详情页显示「当前轮」）。没有任何轮次时返回 ErrNotFound。
func (r *Repo) LatestTurn(ctx context.Context, sessionID int64) (Turn, error) {
	return scanTurn(r.pool.QueryRow(ctx, `select `+turnCols+` from agent_turn
		where session_id = $1 order by created_at desc, id desc limit 1`, sessionID))
}

// UserNames 取用户昵称（会话列表显示发起人）。查不到的不出现在结果里。
//
// ⚠️ 只读一列展示用的昵称，与 canvas/repo 显示「最后编辑者」的 left join 同一做法；
// 不读身份域的任何其他字段。
func (r *Repo) UserNames(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `select id, coalesce(nickname, '') from app_user where id = any($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// Pool 返回连接池（作为 Querier），给「不在事务里」的单条写入用。
func (r *Repo) Pool() Querier { return r.pool }
