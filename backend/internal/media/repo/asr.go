package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// 识别结果的分析行（`media_analysis kind='asr'`，docs/agent/smartcut SCT-33，SCD-32）。
//
// 一行一个（源素材，引擎 = 模型码，参数 = 是否保留口癖 + 语言）。与扫描器做的那几种不同：识别是一次按模型
// 收费的生成任务，扫描器不领它（domain.ScannerKinds 里没有 asr）。状态：
//
//	queued  已请求、还没受理（在等识别用音轨；result 里是受理要的请求信息）
//	running 已受理，generation_id 是那次识别任务
//	success 结果在 result；failed 记原因（不在唯一索引里，所以失败之后可以重新请求）
//
// ⚠️ 排队与进行中也唯一（uk_media_analysis）：两个并发的识别请求只能有一个插得进去 —— 另一个看到已有的那一行，
// 不再受理、不再扣费。只在 success 上唯一的话，两个请求会各建一个任务、各扣一次费。

// SpeechAudioState 是一份素材「识别用音轨」（speech_audio）的当前状态。
type SpeechAudioState struct {
	Status     string // queued / running / success / failed；没有行时为空
	DerivedID  int64  // 抽好的音轨（success 时）
	DurationMs int
	NoAudio    bool
	Error      string
}

// SpeechAudio 取一批素材的识别用音轨状态（当前那一行：非失败的优先，没有再取最近一次失败的）。归属在 SQL 里。
func (r *Repo) SpeechAudio(ctx context.Context, workspaceID int64, mediaIDs []int64, engine string) (
	map[int64]SpeechAudioState, error) {
	out := map[int64]SpeechAudioState{}
	if len(mediaIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		select distinct on (a.media_object_id) a.media_object_id, a.status, coalesce(a.derived_media_id,0),
		       a.result, coalesce(a.error_message,'')
		from media_analysis a
		where a.workspace_id = $1 and a.media_object_id = any($2) and a.kind = 'speech_audio' and a.engine = $3
		order by a.media_object_id, (a.status = 'failed'), a.updated_at desc, a.id desc`,
		workspaceID, mediaIDs, engine)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var s SpeechAudioState
		var raw []byte
		if err := rows.Scan(&id, &s.Status, &s.DerivedID, &raw, &s.Error); err != nil {
			return nil, err
		}
		var res struct {
			DurationMs int  `json:"durationMs"`
			NoAudio    bool `json:"noAudio"`
		}
		if len(raw) > 0 && json.Unmarshal(raw, &res) == nil {
			s.DurationMs, s.NoAudio = res.DurationMs, res.NoAudio
		}
		out[id] = s
	}
	return out, rows.Err()
}

// ASRRows 取一批素材在某组参数下**非失败**的识别行（最多一行，唯一索引保证）。
func (r *Repo) ASRRows(ctx context.Context, workspaceID int64, mediaIDs []int64, engine, paramsKey string) (
	map[int64]Analysis, error) {
	out := map[int64]Analysis{}
	if len(mediaIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `select `+analysisCols+` from media_analysis
		where workspace_id = $1 and media_object_id = any($2) and kind = 'asr' and engine = $3 and params_key = $4
		  and status <> 'failed'`, workspaceID, mediaIDs, engine, paramsKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAnalysis(rows)
		if err != nil {
			return nil, err
		}
		out[a.MediaObjectID] = a
	}
	return out, rows.Err()
}

// InsertASR 排一行识别（queued；pending 是受理要的请求信息）。同键已有非失败的行时不插，返回那一行与 false。
// 素材不在本工作区或已删除时返回 ErrNotFound。
func (r *Repo) InsertASR(ctx context.Context, id, workspaceID, mediaID int64, engine, paramsKey string,
	pending json.RawMessage) (Analysis, bool, error) {
	a, err := scanAnalysis(r.pool.QueryRow(ctx, `
		insert into media_analysis (id, workspace_id, media_object_id, kind, engine, params_key, status, result)
		select $1, m.workspace_id, m.id, 'asr', $4, $5, 'queued', $6
		from media_object m where m.id = $3 and m.workspace_id = $2 and m.status = 'active'
		on conflict (media_object_id, kind, engine, params_key) where status <> 'failed' do nothing
		returning `+analysisCols, id, workspaceID, mediaID, engine, paramsKey, pending))
	if err == nil {
		return a, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Analysis{}, false, err
	}
	existing, err := r.ASRRows(ctx, workspaceID, []int64{mediaID}, engine, paramsKey)
	if err != nil {
		return Analysis{}, false, err
	}
	if row, ok := existing[mediaID]; ok {
		return row, false, nil
	}
	return Analysis{}, false, ErrNotFound
}

// AttachASR 受理之后把任务挂上：queued → running（CAS：只有还没挂任务的 queued 行）。
func (r *Repo) AttachASR(ctx context.Context, id, generationID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update media_analysis set status = 'running', generation_id = $2, result = null, lease_until = null,
		       updated_at = now()
		where id = $1 and kind = 'asr' and status = 'queued' and generation_id is null`, id, generationID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FailASR 记一行识别失败（受理没成、音轨抽不出来）。只改 queued / running 的。
func (r *Repo) FailASR(ctx context.Context, id int64, msg string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update media_analysis set status = 'failed', error_message = left($2, 512), lease_until = null, updated_at = now()
		where id = $1 and kind = 'asr' and status in ('queued','running')`, id, msg)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SucceedASR 直接把一行记成成功（没有音轨的素材：不受理、不收费，结果是「没有语音」）。
func (r *Repo) SucceedASR(ctx context.Context, id int64, result json.RawMessage) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update media_analysis set status = 'success', result = $2, lease_until = null, updated_at = now()
		where id = $1 and kind = 'asr' and status = 'queued'`, id, result)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FailedASRCount 某素材某组参数失败过几次（识别的幂等键按它算第几次：失败之后要换新键，SC-AC-50）。
func (r *Repo) FailedASRCount(ctx context.Context, mediaID int64, engine, paramsKey string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `select count(*) from media_analysis
		where media_object_id = $1 and kind = 'asr' and engine = $2 and params_key = $3 and status = 'failed'`,
		mediaID, engine, paramsKey).Scan(&n)
	return n, err
}

// PendingASR 一行等着受理的识别（queued、没挂任务），连同它那份素材的识别用音轨状态。
type PendingASR struct {
	Analysis
	Speech SpeechAudioState
}

// ListPendingASR 列出等着受理的识别（最早的在前）。不在这里领：领取用 ClaimPendingASR 逐行 CAS。
func (r *Repo) ListPendingASR(ctx context.Context, speechEngine string, limit int) ([]PendingASR, error) {
	rows, err := r.pool.Query(ctx, `
		select `+prefixed("a")+`, coalesce(s.status,''), coalesce(s.derived_media_id,0), s.result, coalesce(s.error_message,'')
		from media_analysis a
		left join lateral (
		       select status, derived_media_id, result, error_message from media_analysis s
		        where s.media_object_id = a.media_object_id and s.kind = 'speech_audio' and s.engine = $1
		        order by (s.status = 'failed'), s.updated_at desc, s.id desc limit 1) s on true
		where a.kind = 'asr' and a.status = 'queued' and a.generation_id is null
		  and (a.lease_until is null or a.lease_until < now())
		order by a.created_at, a.id
		limit $2`, speechEngine, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingASR
	for rows.Next() {
		var p PendingASR
		var raw []byte
		a := &p.Analysis
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.MediaObjectID, &a.Kind, &a.Engine, &a.ParamsKey, &a.Status,
			&a.Attempts, &a.LeaseUntil, &a.Result, &a.DerivedMediaID, &a.GenerationID, &a.ErrorMessage,
			&a.CreatedAt, &a.UpdatedAt, &p.Speech.Status, &p.Speech.DerivedID, &raw, &p.Speech.Error); err != nil {
			return nil, err
		}
		var res struct {
			DurationMs int  `json:"durationMs"`
			NoAudio    bool `json:"noAudio"`
		}
		if len(raw) > 0 && json.Unmarshal(raw, &res) == nil {
			p.Speech.DurationMs, p.Speech.NoAudio = res.DurationMs, res.NoAudio
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ClaimPendingASR 领一行等着受理的识别（写一个短租约）：API 多实例同时扫时只有一个去受理。
// 受理本身也是幂等的（同一把键），这一步只省掉重复的报价与受理调用。
func (r *Repo) ClaimPendingASR(ctx context.Context, id int64, lease time.Duration) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update media_analysis set lease_until = now() + $2::interval, updated_at = now()
		where id = $1 and kind = 'asr' and status = 'queued' and generation_id is null
		  and (lease_until is null or lease_until < now())`, id, interval(lease))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FinishASR 识别任务到终态时回写（SmartcutBinder 调）。
//
// 先按任务 ID 找挂着它的那一行；找不到（运维重试出来的新任务、那一行已经被判失败）时：成功就按键补一行成功的
// （已有成功的不覆盖），失败就什么都不做。返回是否改动了一行。
func (r *Repo) FinishASR(ctx context.Context, id, generationID, workspaceID, mediaID int64, engine, paramsKey,
	status string, result json.RawMessage, msg string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update media_analysis set status = $2::text,
		       result = case when $2::text = 'success' then $3::jsonb else result end,
		       error_message = nullif(left($4::text, 512), ''), lease_until = null, updated_at = now()
		where generation_id = $1 and kind = 'asr' and status in ('queued','running')`,
		generationID, status, result, msg)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 || status != "success" {
		return tag.RowsAffected() > 0, nil
	}
	tag, err = r.pool.Exec(ctx, `
		insert into media_analysis (id, workspace_id, media_object_id, kind, engine, params_key, status, result, generation_id)
		select $1, m.workspace_id, m.id, 'asr', $4, $5, 'success', $6, $7
		from media_object m where m.id = $3 and m.workspace_id = $2
		on conflict (media_object_id, kind, engine, params_key) where status <> 'failed' do update
		   set status = 'success', result = excluded.result, generation_id = excluded.generation_id,
		       error_message = null, lease_until = null, updated_at = now()
		 where media_analysis.status <> 'success'`, id, workspaceID, mediaID, engine, paramsKey, result, generationID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// prefixed 给 analysisCols 加表别名（联表查询时用）。
func prefixed(alias string) string {
	return alias + `.id, ` + alias + `.workspace_id, ` + alias + `.media_object_id, ` + alias + `.kind, ` +
		alias + `.engine, ` + alias + `.params_key, ` + alias + `.status, ` + alias + `.attempts, ` +
		alias + `.lease_until, ` + alias + `.result, coalesce(` + alias + `.derived_media_id,0), coalesce(` +
		alias + `.generation_id,0), coalesce(` + alias + `.error_message,''), ` + alias + `.created_at, ` +
		alias + `.updated_at`
}

// ASRSuccessRows 一批素材全部成功的识别行（最新的在前）：用哪一份由调用方按用途挑（口播要保留口癖且有字级时间）。
func (r *Repo) ASRSuccessRows(ctx context.Context, workspaceID int64, mediaIDs []int64) ([]Analysis, error) {
	if len(mediaIDs) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `select `+analysisCols+` from media_analysis
		where workspace_id = $1 and media_object_id = any($2) and kind = 'asr' and status = 'success' and result is not null
		order by updated_at desc, id desc`, workspaceID, mediaIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Analysis
	for rows.Next() {
		a, err := scanAnalysis(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
