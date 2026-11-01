package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Output 是一条产物。
type Output struct {
	Idx           int
	OutputType    string
	MediaObjectID int64
	TextContent   string
	Meta          json.RawMessage
}

// SaveOutputsAndFinish 归档产物并标成功，**同一个事务**。
//
// ⚠️ **先归档输出再标成功**（GO-23）：反过来的话，存储失败时用户已经看到
// "成功"却取不到产物 —— 那是最糟的一种失败，因为它既不会触发退款，
// 也不会被重试捞起来。
//
// ⚠️ 状态更新仍带 `where status in ('queued','running')`：
// 任务可能在归档期间被取消或被恢复扫描判超时。返回 false 时调用方
// **不要发通知、不要推事件** —— 那个终态已经有人处理过了。
func (r *Repo) SaveOutputsAndFinish(ctx context.Context, id int64, nextID func() int64,
	outputs []Output, raw json.RawMessage) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 先占住终态：任务已被取消时这里影响 0 行，产物就不必白写了。
	tag, err := tx.Exec(ctx, `
		update generation
		set status = 'success', progress = 100, provider_raw_result = $2,
		    finished_at = now(), updated_at = now()
		where id = $1 and status in ('queued','running')`, id, raw)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	for _, o := range outputs {
		meta := o.Meta
		if len(meta) == 0 {
			meta = json.RawMessage(`{}`)
		}
		// ⚠️ `on conflict (generation_id, idx) do nothing`：重投的消息可能
		// 把同一批产物再归档一次，唯一索引让第二次成为无操作而不是报错。
		// ⚠️ 文本产物的空串**照存**，不能 nullif 成 NULL（docs/agent/smartcut SCB-12）：ck_output_payload 要求
		// 文本产物 text_content 非空，静音素材的识别结果就是空串 —— 写成 NULL 的话整条落库回滚，
		// 执行器判可重试、一路重试到死线，用户看到的是「识别超时」。媒体产物照旧 nullif（它本来就没有文本）。
		if _, err := tx.Exec(ctx, `
			insert into generation_output (id, generation_id, idx, media_object_id,
				output_type, text_content, meta)
			values ($1,$2,$3,nullif($4::bigint,0),$5::text,
			        case when $5::text = 'text' then $6::text else nullif($6::text,'') end,$7)
			on conflict (generation_id, idx) do nothing`,
			nextID(), id, o.Idx, o.MediaObjectID, o.OutputType, o.TextContent, meta); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// ListOutputs 取一个任务的全部产物。
func (r *Repo) ListOutputs(ctx context.Context, generationID int64) ([]Output, error) {
	rows, err := r.pool.Query(ctx, `
		select idx, output_type, coalesce(media_object_id,0), coalesce(text_content,''), meta
		from generation_output where generation_id = $1 order by idx`, generationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Output
	for rows.Next() {
		var o Output
		if err := rows.Scan(&o.Idx, &o.OutputType, &o.MediaObjectID, &o.TextContent, &o.Meta); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetOutput 取指定序号的产物，**带工作区归属校验**。
func (r *Repo) GetOutput(ctx context.Context, workspaceID, generationID int64, idx int) (
	Output, error) {
	var o Output
	err := r.pool.QueryRow(ctx, `
		select o.idx, o.output_type, coalesce(o.media_object_id,0),
		       coalesce(o.text_content,''), o.meta
		from generation_output o
		join generation g on g.id = o.generation_id
		where o.generation_id = $1 and o.idx = $2 and g.workspace_id = $3`,
		generationID, idx, workspaceID).
		Scan(&o.Idx, &o.OutputType, &o.MediaObjectID, &o.TextContent, &o.Meta)
	if errors.Is(err, pgx.ErrNoRows) {
		return Output{}, ErrNotFound
	}
	return o, err
}

// Cancel 取消任务。
//
// ⚠️ 新系统用**独立的 cancelled 状态**，不是旧系统的 `failed + errorCode=cancelled`（CD-06）。
// 同样带状态条件：已经跑完的任务取消不了 —— 那时候钱已经花在上游了，
// 假装取消成功只会让用户以为不用付费。
func (r *Repo) Cancel(ctx context.Context, workspaceID, id int64, reason string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update generation
		set status = 'cancelled', error_code = 'cancelled_by_user',
		    error_message = nullif($3,''), finished_at = now(), updated_at = now()
		where id = $1 and workspace_id = $2 and status in ('queued','running')`,
		id, workspaceID, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// InsertNotification 发一条站内通知。
//
// ⚠️ 幂等靠 `uk_notification_ref(user_id, type, ref_type, ref_id)`：
// Worker 重试同一个任务不会发第二条（FLOW-10）。
func (r *Repo) InsertNotification(ctx context.Context, id, userID int64,
	typ, title, content, refType, refID, linkURL string) error {
	_, err := r.pool.Exec(ctx, `
		insert into notification (id, user_id, type, title, content, ref_type, ref_id, link_url)
		values ($1,$2,$3,$4,nullif($5,''),nullif($6,''),nullif($7,''),nullif($8,''))
		on conflict (user_id, type, ref_type, ref_id) where ref_id is not null do nothing`,
		id, userID, typ, title, content, refType, refID, linkURL)
	return err
}

// InputFact 是智能剪辑素材条要的一次生成（SCT-25）：状态、操作码、失败原因与全部产物的媒体（按 idx）。
type InputFact struct {
	Status         string
	Operation      string
	ErrorMessage   string
	OutputMediaIDs []int64
}

// InputFacts 一次回查一批生成。⚠️ 归属在 SQL 里：只返回本工作区、本项目的，查不到的不在结果里
// （调用方记「不是本项目的生成」）—— 画布是客户端存的，挂在节点上的生成 ID 不能信（SCK-16）。
// 产物只取有媒体的（文本产出不进素材条的媒体），按 idx 排序。
func (r *Repo) InputFacts(ctx context.Context, workspaceID, projectID int64, ids []int64) (map[int64]InputFact, error) {
	out := map[int64]InputFact{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		select g.id, g.status, coalesce(o.code, ''), coalesce(g.error_message, ''),
		       coalesce((select array_agg(x.media_object_id order by x.idx) from generation_output x
		                  where x.generation_id = g.id and x.media_object_id is not null), '{}'::bigint[])
		from generation g left join operation o on o.id = g.operation_id
		where g.id = any($3) and g.workspace_id = $1 and g.project_id = $2`, workspaceID, projectID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var f InputFact
		if err := rows.Scan(&id, &f.Status, &f.Operation, &f.ErrorMessage, &f.OutputMediaIDs); err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, rows.Err()
}

// PromptsByOutputMedia 产出这些媒体的那次（成功的）生成的提示词，按媒体 ID（智能剪辑的素材描述，SC-F-80：
// 生成的素材用已有的生成提示词理解，不必再看图）。同一媒体多次出现时取最近一次。查不到 / 没写提示词的不在结果里。
func (r *Repo) PromptsByOutputMedia(ctx context.Context, workspaceID int64, mediaIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(mediaIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		select distinct on (o.media_object_id) o.media_object_id, trim(g.params_snapshot->>'prompt')
		from generation_output o
		join generation g on g.id = o.generation_id and g.workspace_id = $1 and g.status = 'success'
		where o.media_object_id = any($2) and coalesce(trim(g.params_snapshot->>'prompt'), '') <> ''
		order by o.media_object_id, g.created_at desc, g.id desc`, workspaceID, mediaIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var prompt string
		if err := rows.Scan(&id, &prompt); err != nil {
			return nil, err
		}
		out[id] = prompt
	}
	return out, rows.Err()
}

// OutputSource 产出某份媒体的那次生成（最近一次成功的）：操作码、模型、节点自己的提示词、参数与输入快照。
type OutputSource struct {
	GenerationID  int64
	ProjectID     int64
	OperationCode string
	ModelCode     string
	ModelName     string
	Prompt        string
	// Params 参数快照原样（含 `_snapshot`）；Inputs 输入快照原样。
	Params json.RawMessage
	Inputs json.RawMessage
	// Origin 快照里的来源（空 = 常规；smartcut = 智能剪辑名下的片段级生成）。
	Origin string
	// SmartcutKind / SmartcutTool 智能剪辑名下的任务的回写种类（replace / insert …）与工具（regen / fill / desubtitle …）；
	// 不是时为空。
	SmartcutKind string
	SmartcutTool string
}

// SourcesByOutputMedia 一批媒体各自是哪次成功的生成产出的（最近一次），带操作码、提示词与输入快照（原样）。
// 归属在 SQL 里：只认本工作区的生成。智能剪辑的解说成片用它还原配音的原稿（docs/agent/smartcut SCT-35）——
// 快照是那次生成实际念的内容；画布上的节点生成之后可能又被改过。
func (r *Repo) SourcesByOutputMedia(ctx context.Context, workspaceID int64, mediaIDs []int64) (map[int64]OutputSource, error) {
	out := map[int64]OutputSource{}
	if len(mediaIDs) == 0 {
		return out, nil
	}
	// ⚠️ 模型用 left join：模型被删之后这次生成照样是这份素材的来源（生成信息里显示「模型已下线」）。
	rows, err := r.pool.Query(ctx, `
		select distinct on (o.media_object_id) o.media_object_id, g.id, g.project_id, coalesce(op.code, ''),
		       coalesce(m.code, ''), coalesce(m.name, ''),
		       coalesce(trim(g.params_snapshot->>'prompt'), ''), g.params_snapshot, coalesce(g.inputs_snapshot, '[]'::jsonb),
		       coalesce(g.params_snapshot->'_snapshot'->>'origin', ''), coalesce(g.smartcut_target->>'kind', ''),
		       coalesce(g.smartcut_target->>'tool', '')
		from generation_output o
		join generation g on g.id = o.generation_id and g.workspace_id = $1 and g.status = 'success'
		left join operation op on op.id = g.operation_id
		left join model m on m.id = g.model_id
		where o.media_object_id = any($2)
		order by o.media_object_id, g.created_at desc, g.id desc`, workspaceID, mediaIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var src OutputSource
		var params, inputs []byte
		if err := rows.Scan(&id, &src.GenerationID, &src.ProjectID, &src.OperationCode, &src.ModelCode, &src.ModelName,
			&src.Prompt, &params, &inputs, &src.Origin, &src.SmartcutKind, &src.SmartcutTool); err != nil {
			return nil, err
		}
		src.Params, src.Inputs = params, inputs
		out[id] = src
	}
	return out, rows.Err()
}
