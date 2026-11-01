package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// Card 是 agent_card 的一行。SubmittedBy 为 0 表示未提交。
type Card struct {
	ID          int64
	SessionID   int64
	StepID      int64
	Kind        domain.CardKind
	Payload     json.RawMessage
	Status      domain.CardStatus
	Submission  json.RawMessage
	SubmittedBy int64
	SubmittedAt *time.Time
	CreatedAt   time.Time
}

// Domain 转成领域层校验用的卡片快照。
func (c Card) Domain() domain.Card { return domain.Card{Kind: c.Kind, Payload: c.Payload} }

const cardCols = `id, session_id, step_id, kind, payload, status, submission,
	coalesce(submitted_by, 0), submitted_at, created_at`

func scanCard(row pgx.Row) (Card, error) {
	var c Card
	err := row.Scan(&c.ID, &c.SessionID, &c.StepID, &c.Kind, &c.Payload, &c.Status, &c.Submission,
		&c.SubmittedBy, &c.SubmittedAt, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Card{}, ErrNotFound
	}
	return c, err
}

// InsertCard 发一张卡片（status=pending）。通常与会话状态迁移在同一事务。
func (r *Repo) InsertCard(ctx context.Context, q Querier, c Card) (Card, error) {
	out, err := scanCard(q.QueryRow(ctx, `
		insert into agent_card (id, session_id, step_id, kind, payload, status)
		values ($1, $2, $3, $4, $5, 'pending')
		returning `+cardCols, c.ID, c.SessionID, c.StepID, c.Kind, jsonOr(c.Payload, "{}")))
	if err != nil {
		return Card{}, mapDuplicate(err)
	}
	return out, nil
}

// GetCard 按会话取卡片（会话归属由调用方先校验）。
func (r *Repo) GetCard(ctx context.Context, sessionID, id int64) (Card, error) {
	return scanCard(r.pool.QueryRow(ctx, `select `+cardCols+` from agent_card
		where id = $1 and session_id = $2`, id, sessionID))
}

// SubmitCard CAS pending → submitted，记提交内容与提交人。
//
// 重复提交只有一次 changed=true（AG-06「重复提交幂等」）：调用方据此决定要不要建轮次，
// 第二次直接返回已提交的结果。
func (r *Repo) SubmitCard(ctx context.Context, q Querier, id int64, submission json.RawMessage,
	userID int64) (bool, error) {
	tag, err := q.Exec(ctx, `
		update agent_card set status = 'submitted', submission = $2, submitted_by = $3, submitted_at = now()
		where id = $1 and status = 'pending'`, id, jsonOr(submission, "{}"), userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// VoidCard CAS pending → voided（规划确认卡被跳过、旧规划作废）。
func (r *Repo) VoidCard(ctx context.Context, q Querier, id int64) (bool, error) {
	tag, err := q.Exec(ctx, `update agent_card set status = 'voided' where id = $1 and status = 'pending'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// PendingBySession 列出会话里待处理的卡片（创建顺序）。
func (r *Repo) PendingBySession(ctx context.Context, sessionID int64) ([]Card, error) {
	rows, err := r.pool.Query(ctx, `select `+cardCols+` from agent_card
		where session_id = $1 and status = 'pending' order by created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	return collectCards(rows)
}

// AnsweredQuestionCards 列出会话里已提交的问答卡，最新提交的在前（最多 limit 张）。
// 视频规格三问（画幅 / 清晰度 / 时长）按固定题目 id 从这里取回答（domain.SpecAnswers）。
func (r *Repo) AnsweredQuestionCards(ctx context.Context, sessionID int64, limit int) ([]Card, error) {
	rows, err := r.pool.Query(ctx, `select `+cardCols+` from agent_card
		where session_id = $1 and kind = 'questions' and status = 'submitted'
		order by submitted_at desc nulls last, id desc limit $2`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	return collectCards(rows)
}

// ExpireOlderThan 把创建超过 age 的 pending 卡片标 expired，返回被过期的卡片
// （扫描循环据此把会话回 idle）。只处理 pending：已提交 / 已作废的不动。
//
// `for update skip locked`：多实例同时扫时各拿各的，不互相等待。
func (r *Repo) ExpireOlderThan(ctx context.Context, age time.Duration, limit int) ([]Card, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
		update agent_card set status = 'expired'
		where id in (
			select id from agent_card
			where status = 'pending' and created_at < now() - $1::bigint * interval '1 millisecond'
			order by created_at limit $2
			for update skip locked)
		and status = 'pending'
		returning `+cardCols, age.Milliseconds(), limit)
	if err != nil {
		return nil, err
	}
	return collectCards(rows)
}

func collectCards(rows pgx.Rows) ([]Card, error) {
	defer rows.Close()
	var out []Card
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LatestApprovedPlan 取会话里最近一张**已批准**的规划确认卡（MKT-T-22：营销会话的 generate 按条目键取条目）。
//
// 「已批准」= 已提交且决定为 approve；作废 / 过期 / 被「需要调整」的不算。没有时返回 ErrNotFound。
// 卡片 payload 在批准事务里已写成换图后的条目（MKT-T-20），所以这里读到的就是用户确认过的那一份。
func (r *Repo) LatestApprovedPlan(ctx context.Context, sessionID int64) (Card, error) {
	return scanCard(r.pool.QueryRow(ctx, `select `+cardCols+` from agent_card
		where session_id = $1 and kind = 'plan_confirm' and status = 'submitted'
			and submission->>'decision' = 'approve'
		order by submitted_at desc nulls last, id desc limit 1`, sessionID))
}
