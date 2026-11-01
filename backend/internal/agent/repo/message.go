package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 消息角色（与 ck_agent_message_role 逐字一致）。
const (
	RoleUser       = "user"
	RoleAssistant  = "assistant"
	RoleTool       = "tool"
	RoleSystemNote = "system_note"
	// RoleSystemEvent 是系统事件（如所等任务的结局）：进模型上下文（唤醒轮靠它知道成败与产出）。
	// 2026-10-25 起面板上不显示（design/03 R-8）：任务成败与产出在步骤行与产出卡片里已经看得到。
	// 与 RoleSystemNote 的区别只在这一点 —— 见迁移 00045 的起因。
	RoleSystemEvent = "system_event"
)

// Message 是 agent_message 的一行。TurnID / CardID 为 0 表示没有。
type Message struct {
	ID         int64
	SessionID  int64
	TurnID     int64
	Seq        int
	Role       string
	Content    string
	Refs       json.RawMessage
	ToolCalls  json.RawMessage // nil = 没有工具调用（落库为 null）
	ToolCallID string
	CardID     int64
	CreatedAt  time.Time
}

const messageCols = `id, session_id, coalesce(turn_id, 0), seq, role, content, refs, tool_calls,
	coalesce(tool_call_id, ''), coalesce(card_id, 0), created_at`

func scanMessage(row pgx.Row) (Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.SessionID, &m.TurnID, &m.Seq, &m.Role, &m.Content, &m.Refs,
		&m.ToolCalls, &m.ToolCallID, &m.CardID, &m.CreatedAt)
	return m, err
}

// Append 追加一条消息，返回带 seq 的那一行。
//
// ⚠️ seq = 会话内 `coalesce(max(seq),0)+1`，**依赖会话行锁**：这里先 `for update` 锁会话行
// （调用方已经锁过也没关系，同一事务里重复加锁是空操作），并发追加因此排队、seq 连续无洞。
// 不锁的话两个事务读到同一个 max，uk_agent_message_seq 报 23505 —— 看起来像随机失败。
func (r *Repo) Append(ctx context.Context, tx pgx.Tx, m Message) (Message, error) {
	var locked int64
	err := tx.QueryRow(ctx, `select id from agent_session where id = $1 for update`, m.SessionID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	var toolCalls any
	if len(m.ToolCalls) > 0 {
		toolCalls = m.ToolCalls
	}
	out, err := scanMessage(tx.QueryRow(ctx, `
		insert into agent_message (id, session_id, turn_id, seq, role, content, refs, tool_calls,
			tool_call_id, card_id)
		values ($1, $2, nullif($3::bigint, 0),
			(select coalesce(max(seq), 0) + 1 from agent_message where session_id = $2),
			$4, $5, $6, $7, $8, nullif($9::bigint, 0))
		returning `+messageCols,
		m.ID, m.SessionID, m.TurnID, m.Role, m.Content, jsonOr(m.Refs, "[]"), toolCalls,
		nullStr(m.ToolCallID), m.CardID))
	if err != nil {
		return Message{}, mapDuplicate(err)
	}
	if _, err := tx.Exec(ctx, `update agent_session set last_active_at = now() where id = $1`,
		m.SessionID); err != nil {
		return Message{}, err
	}
	return out, nil
}

// ListPage 按 seq 游标往前翻页：取 seq < cursorSeq 的最近 limit 条（cursorSeq ≤ 0 = 从最新开始），
// 结果按 seq **升序**返回。nextCursor 是本页最小 seq，0 表示没有更早的了。
func (r *Repo) ListPage(ctx context.Context, sessionID int64, cursorSeq, limit int) ([]Message, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `select `+messageCols+` from agent_message
		where session_id = $1 and ($2 <= 0 or seq < $2)
		order by seq desc limit $3`, sessionID, cursorSeq, limit+1)
	if err != nil {
		return nil, 0, err
	}
	msgs, err := collectMessages(rows)
	if err != nil {
		return nil, 0, err
	}
	next := 0
	if len(msgs) > limit {
		msgs = msgs[:limit]
		next = msgs[len(msgs)-1].Seq
	}
	reverse(msgs)
	return msgs, next, nil
}

// ListRecent 取最近 limit 条，按 seq 升序（组装上下文用）。
func (r *Repo) ListRecent(ctx context.Context, sessionID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `select `+messageCols+` from agent_message
		where session_id = $1 order by seq desc limit $2`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	msgs, err := collectMessages(rows)
	if err != nil {
		return nil, err
	}
	reverse(msgs)
	return msgs, nil
}

func collectMessages(rows pgx.Rows) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
