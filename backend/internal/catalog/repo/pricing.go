package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
)

func (r *Repo) ListPrices(ctx context.Context) ([]domain.Price, error) {
	rows, err := r.pool.Query(ctx, `
		select o.code, coalesce(m.code,''), p.credits, p.unit, p.status = 'enabled'
		from pricing p
		join operation o on o.id = p.operation_id
		left join model m on m.id = p.model_id
		order by o.code, coalesce(m.code,'')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Price
	for rows.Next() {
		var p domain.Price
		if err := rows.Scan(&p.OperationCode, &p.ModelCode, &p.Credits, &p.Unit, &p.Enabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SavePrice 写价格。modelCode 为空即 operation 级兜底价。
//
// ⚠️ 唯一索引用了 `nulls not distinct`，让 (op, null) 这条兜底价
// 也受唯一约束保护 —— 否则同一个 operation 可以有多条兜底价，
// 报价时选中哪条全看查询顺序。
func (r *Repo) SavePrice(ctx context.Context, id int64, p domain.Price) error {
	var modelID any
	if p.ModelCode != "" {
		var mid int64
		err := r.pool.QueryRow(ctx, `select id from model where code = $1`, p.ModelCode).Scan(&mid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		modelID = mid
	}
	status := "disabled"
	if p.Enabled {
		status = "enabled"
	}
	tag, err := r.pool.Exec(ctx, `
		insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, $3, $4, $5, $6 from operation o where o.code = $2
		on conflict (operation_id, model_id) do update
		set credits = excluded.credits, unit = excluded.unit,
		    status = excluded.status, updated_at = now()`,
		id, p.OperationCode, modelID, p.Credits, string(p.Unit), status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) DeletePrice(ctx context.Context, operationCode, modelCode string) error {
	var tag interface{ RowsAffected() int64 }
	var err error
	if modelCode == "" {
		tag, err = r.pool.Exec(ctx, `
			delete from pricing p using operation o
			where p.operation_id = o.id and o.code = $1 and p.model_id is null`, operationCode)
	} else {
		tag, err = r.pool.Exec(ctx, `
			delete from pricing p using operation o, model m
			where p.operation_id = o.id and p.model_id = m.id
			  and o.code = $1 and m.code = $2`, operationCode, modelCode)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MemberLevelExists 判断会员等级是否存在且启用。
func (r *Repo) MemberLevelExists(ctx context.Context, levelID int64) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx,
		`select exists(select 1 from member_level where id = $1 and is_enabled)`, levelID).Scan(&ok)
	return ok, err
}

// QuoteFacts 是一次报价所需的全部配置事实，一次查询取齐。
type QuoteFacts struct {
	ModelID          int64
	ModelEst         int
	OperationEst     *int
	OperationEnabled bool
	ModelOpEnabled   bool
	ModelEnabled     bool
	ProviderReady    bool
	MinMemberLevelID *int64
	MaxInputChars    *int
	OutNodeType      string
	Found            bool
	// 以下四项供生成受理冻结进任务快照 —— 事后改配置不得改变历史任务的解释。
	OperationID  int64
	ProviderCode string
	APIProtocol  string
	ResultType   string
}

// LoadQuoteFacts 取报价所需的全部事实。
//
// 一次查询取齐而不是分多次：报价在生成受理的热路径上，
// 分开查会让一次生成多出四五个 round trip。
func (r *Repo) LoadQuoteFacts(ctx context.Context, modelCode, operationCode string) (QuoteFacts, error) {
	var f QuoteFacts
	err := r.pool.QueryRow(ctx, `
		select m.id, m.est_seconds, mo.est_seconds,
		       o.status = 'enabled', mo.is_enabled, m.status = 'enabled',
		       (p.status = 'enabled' and (p.auth_type = 'none' or p.api_key_cipher is not null)),
		       m.min_member_level_id, m.max_input_chars, o.out_node_type,
		       o.id, p.code, m.api_protocol, o.domain
		from model m
		join provider_config p on p.id = m.provider_id
		join model_operation mo on mo.model_id = m.id
		join operation o on o.id = mo.operation_id
		where m.code = $1 and o.code = $2`, modelCode, operationCode).
		Scan(&f.ModelID, &f.ModelEst, &f.OperationEst, &f.OperationEnabled,
			&f.ModelOpEnabled, &f.ModelEnabled, &f.ProviderReady,
			&f.MinMemberLevelID, &f.MaxInputChars, &f.OutNodeType,
			&f.OperationID, &f.ProviderCode, &f.APIProtocol, &f.ResultType)
	if errors.Is(err, pgx.ErrNoRows) {
		return QuoteFacts{}, nil // Found = false
	}
	if err != nil {
		return QuoteFacts{}, err
	}
	f.Found = true
	return f, nil
}
