package repo

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// OutboxAggregateType 是 agent 写进 outbox_message 的 aggregate_type。
const OutboxAggregateType = "agent"

// EnqueueTurnInTx 在业务事务里写一条「请 Worker 跑这一轮」的 outbox 消息。
//
// ⚠️ outbox_message 是**平台表**（迁移 00013），agent 用**自己的 repo** 写它，
// 不调用 generation/repo.InsertOutboxInTx（dev/01 §4.2）；已有的投递器认领时不按
// aggregate_type 过滤，所以不需要第二个投递器。
// ⚠️ 与「建轮次 / 写消息」同一事务：事务回滚则消息不留，提交则一定会发（GO-13）。
func (r *Repo) EnqueueTurnInTx(ctx context.Context, tx pgx.Tx, outboxID, sessionID, turnID int64) error {
	payload, err := json.Marshal(domain.TurnRequestedPayload{
		SessionID: strconv.FormatInt(sessionID, 10), TurnID: strconv.FormatInt(turnID, 10),
		SchemaVersion: 1,
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		insert into outbox_message (id, aggregate_type, aggregate_id, topic, payload)
		values ($1, $2, $3, $4, $5)`,
		outboxID, OutboxAggregateType, sessionID, domain.TopicTurnRequested, payload)
	return err
}
