package wiring

import (
	"context"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
)

// quickAccepter 把目录域报价、账本余额、生成受理接成画布域「直接生成」的端口（AGT-42）。
//
// ⚠️ 报价走 `QuoteWithFacts`（与 `/generations/estimate`、受理同一条），
// 受理走 `generation/app.Service.Accept` —— 不另写一份，否则「发送前显示的金额」
// 与「账本扣的」会是两个答案（AG-20）。
type quickAccepter struct {
	catalog *catalogapp.Service
	ledger  *billingrepo.Repo
	gen     *genapp.Service
}

func (a quickAccepter) Quote(ctx context.Context, ask canvasapp.QuickQuoteAsk) (canvasapp.QuickQuote, error) {
	q, err := a.catalog.QuoteWithFacts(ctx, catalogapp.QuoteRequest{
		ModelCode: ask.ModelCode, OperationCode: ask.OperationCode,
		Count: ask.Count, DurationSec: ask.DurationSec, Text: ask.Text,
		MemberLevelID: ask.MemberLevelID, RoleCounts: toRoleCount(ask.RoleCounts),
	})
	if err != nil {
		return canvasapp.QuickQuote{}, err
	}
	return canvasapp.QuickQuote{Credits: q.Credits}, nil
}

func (a quickAccepter) Balance(ctx context.Context, payerID int64) (int, error) {
	return a.ledger.Balance(ctx, payerID)
}

func (a quickAccepter) Accept(ctx context.Context, ask canvasapp.QuickAcceptAsk) (canvasapp.QuickAccepted, error) {
	res, err := a.gen.Accept(ctx, genapp.AcceptInput{
		WorkspaceID: ask.WorkspaceID, PayerAccountID: ask.PayerAccountID, UserID: ask.UserID,
		ProjectID: ask.ProjectID, NodeID: ask.NodeID,
		OperationCode: ask.OperationCode, ModelCode: ask.ModelCode,
		IdempotencyKey: ask.IdempotencyKey, MemberLevelID: ask.MemberLevelID,
		Count: ask.Count, DurationSec: ask.DurationSec, Text: ask.Text,
		Params: ask.Params, DeclaredInputs: ask.DeclaredInputs,
	})
	if err != nil {
		return canvasapp.QuickAccepted{}, err
	}
	return canvasapp.QuickAccepted{
		GenerationID: res.Generation.ID, Status: res.Generation.Status,
		Credits: res.Generation.CreditsCost,
	}, nil
}
