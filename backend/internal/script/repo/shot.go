package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
)

// Shot 是镜头行。
type Shot struct {
	ID          int64
	ScriptID    int64
	ShotNo      int
	DurationSec int
	Description string
	ShotSize    string
	Lighting    string
	Dialogue    string
	SoundEffect string
	CameraMove  string
	StartState  string
	EndState    string
	Entities    json.RawMessage
	// 提示词字段由 P07-03 写入，这里只读出来供展示与 stale 判定。
	ImagePrompt            string
	ImagePromptStatus      string
	ImagePromptSource      string
	ImagePromptFingerprint string
	VideoPrompt            string
	VideoPromptStatus      string
	VideoPromptSource      string
	VideoPromptFingerprint string
	Version                int
}

const shotCols = `select id, script_id, shot_no, duration_sec, coalesce(description,''),
	coalesce(shot_size,''), coalesce(lighting,''), coalesce(dialogue,''),
	coalesce(sound_effect,''), coalesce(camera_move,''),
	coalesce(start_state,''), coalesce(end_state,''), entities,
	coalesce(image_prompt,''), image_prompt_status, coalesce(image_prompt_source,''),
	coalesce(image_prompt_fingerprint,''),
	coalesce(video_prompt,''), video_prompt_status, coalesce(video_prompt_source,''),
	coalesce(video_prompt_fingerprint,''),
	version from shot `

func scanShot(row pgx.Row) (Shot, error) {
	var s Shot
	err := row.Scan(&s.ID, &s.ScriptID, &s.ShotNo, &s.DurationSec, &s.Description,
		&s.ShotSize, &s.Lighting, &s.Dialogue, &s.SoundEffect, &s.CameraMove,
		&s.StartState, &s.EndState, &s.Entities,
		&s.ImagePrompt, &s.ImagePromptStatus, &s.ImagePromptSource, &s.ImagePromptFingerprint,
		&s.VideoPrompt, &s.VideoPromptStatus, &s.VideoPromptSource, &s.VideoPromptFingerprint,
		&s.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Shot{}, ErrNotFound
	}
	return s, err
}

func (r *Repo) ListShots(ctx context.Context, scriptID int64) ([]Shot, error) {
	rows, err := r.pool.Query(ctx, shotCols+`where script_id = $1 order by shot_no`, scriptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Shot
	for rows.Next() {
		s, err := scanShot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repo) GetShot(ctx context.Context, scriptID, shotID int64) (Shot, error) {
	return scanShot(r.pool.QueryRow(ctx, shotCols+
		`where id = $1 and script_id = $2`, shotID, scriptID))
}

// ReplaceShots 用一整批新镜头替换脚本的镜头表（分镜生成用）。
//
// ⚠️ 整批替换必须在**一个事务**里：先删后插之间崩掉的话，脚本会变成
// 一个 0 镜头的空壳，而用户已经为这次生成付过费了。
//
// ⚠️ 顺序是「先删再插」而不是 upsert：镜号带唯一索引，
// 新旧两批的镜号会重叠，逐条 upsert 必然撞索引。
func (r *Repo) ReplaceShots(ctx context.Context, scriptID int64,
	nextID func() int64, shots []domain.ParsedShot) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `delete from shot where script_id = $1`, scriptID); err != nil {
		return err
	}
	for _, s := range shots {
		entities, err := json.Marshal(s.Entities)
		if err != nil {
			return err
		}
		if len(s.Entities) == 0 {
			entities = json.RawMessage(`[]`)
		}
		if _, err := tx.Exec(ctx, `
			insert into shot (id, script_id, shot_no, duration_sec, description,
				shot_size, lighting, dialogue, sound_effect, camera_move,
				start_state, end_state, entities)
			values ($1,$2,$3,$4,nullif($5,''),nullif($6,''),nullif($7,''),nullif($8,''),
			        nullif($9,''),nullif($10,''),nullif($11,''),nullif($12,''),$13)`,
			nextID(), scriptID, s.ShotNo, s.DurationSec, s.Description,
			s.ShotSize, s.Lighting, s.Dialogue, s.SoundEffect, s.CameraMove,
			s.StartState, s.EndState, entities); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SaveShotInput 是一次镜头保存。
type SaveShotInput struct {
	ScriptID        int64
	ShotID          int64
	ExpectedVersion int
	DurationSec     *int
	Description     *string
	ShotSize        *string
	Lighting        *string
	Dialogue        *string
	SoundEffect     *string
	CameraMove      *string
	StartState      *string
	EndState        *string
	Entities        json.RawMessage
}

// SaveShot 带版本的镜头更新（AV-10）。
func (r *Repo) SaveShot(ctx context.Context, in SaveShotInput) (Shot, error) {
	tag, err := r.pool.Exec(ctx, `
		update shot set
			duration_sec = coalesce($4, duration_sec),
			description  = coalesce($5, description),
			shot_size    = coalesce($6, shot_size),
			lighting     = coalesce($7, lighting),
			dialogue     = coalesce($8, dialogue),
			sound_effect = coalesce($9, sound_effect),
			camera_move  = coalesce($10, camera_move),
			start_state  = coalesce($11, start_state),
			end_state    = coalesce($12, end_state),
			entities     = coalesce($13, entities),
			version = version + 1, updated_at = now()
		where id = $1 and script_id = $2 and version = $3`,
		in.ShotID, in.ScriptID, in.ExpectedVersion,
		in.DurationSec, in.Description, in.ShotSize, in.Lighting, in.Dialogue,
		in.SoundEffect, in.CameraMove, in.StartState, in.EndState, nilJSON(in.Entities))
	if err != nil {
		return Shot{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := r.GetShot(ctx, in.ScriptID, in.ShotID); err != nil {
			return Shot{}, err
		}
		return Shot{}, ErrVersionConflict
	}
	return r.GetShot(ctx, in.ScriptID, in.ShotID)
}

// ApplyOrder 按给定顺序重排镜号。
//
// ⚠️ 必须**两阶段**：镜号带唯一索引 `(script_id, shot_no)`，
// 直接改成目标号会在中途撞上还没让位的那一行。先把全部镜号挪到
// 一段**低于所有现存值**的临时区，再写目标值。
func (r *Repo) ApplyOrder(ctx context.Context, scriptID int64, orderedIDs []int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := applyOrderTx(ctx, tx, scriptID, orderedIDs); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func applyOrderTx(ctx context.Context, tx pgx.Tx, scriptID int64, orderedIDs []int64) error {
	// ⚠️ 临时号必须**低于所有现存值**，不能只是"取负"或"-row_number()"。
	//
	// PostgreSQL 的唯一索引**不延迟到语句结束才检查**（经典的
	// `update t set i = i + 1` 在 PG 上就会报重复）。所以同一条 UPDATE 里，
	// 只要某一行被赋成另一行**此刻还占着**的值，当场就是 uk_shot_no 冲突。
	//
	// 新插入的镜头带着 -1 这样的负数占位号（插入时目标号还被别人占着），
	// 而 `-row_number()` 也会产出 -1 —— 谁先被更新取决于执行计划，
	// 于是这个 bug **时有时无**：集成测试过了，联调时炸了。
	// 用 `min(shot_no) - row_number()` 把临时区整体推到所有现存值之下，
	// 与执行顺序无关。
	if _, err := tx.Exec(ctx, `
		update shot s set shot_no = t.tmp
		from (select id,
		             (select least(0, min(shot_no)) from shot where script_id = $1)
		               - row_number() over (order by id) as tmp
		        from shot where script_id = $1) t
		where s.id = t.id and s.script_id = $1`, scriptID); err != nil {
		return err
	}
	for i, id := range orderedIDs {
		tag, err := tx.Exec(ctx, `
			update shot set shot_no = $3, updated_at = now()
			where id = $1 and script_id = $2`, id, scriptID, i+1)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
	}
	// 没被点名的镜头留在负数区 = 调用方少传了 ID。宁可报错也不留下
	// 一批镜号为负的行：它们在列表里会排到最前面，而且谁也看不出为什么。
	var leftover int
	if err := tx.QueryRow(ctx,
		`select count(*) from shot where script_id = $1 and shot_no < 0`, scriptID).
		Scan(&leftover); err != nil {
		return err
	}
	if leftover > 0 {
		return errors.New("重排必须包含脚本下的全部镜头")
	}
	return nil
}

// InsertAndReorder 插入若干新镜头并整体重排 —— **一个事务**。
//
// ⚠️ 插入与重排分成两个事务是错的：重排失败时插入已经提交，
// 那一行会永远留在负数镜号上。后果有两个，都很难看懂：
// 它在列表里排到最前面，而且下一次插入会撞上同一个占位号 ——
// **这个脚本从此再也插不进镜头**。实测踩到过（联调第二次跑就炸）。
func (r *Repo) InsertAndReorder(ctx context.Context, scriptID int64,
	inserts []Shot, orderedIDs []int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 插到临时区：目标号此刻还被别人占着，由随后的重排统一分配。
	placeholder := 0
	for _, s := range inserts {
		placeholder--
		entities := s.Entities
		if len(entities) == 0 {
			entities = json.RawMessage(`[]`)
		}
		if _, err := tx.Exec(ctx, `
			insert into shot (id, script_id, shot_no, duration_sec, description,
				shot_size, lighting, dialogue, sound_effect, camera_move,
				start_state, end_state, entities)
			values ($1,$2,$3,$4,nullif($5,''),nullif($6,''),nullif($7,''),nullif($8,''),
			        nullif($9,''),nullif($10,''),nullif($11,''),nullif($12,''),$13)`,
			s.ID, scriptID, tempShotNo(placeholder), s.DurationSec, s.Description,
			s.ShotSize, s.Lighting, s.Dialogue, s.SoundEffect, s.CameraMove,
			s.StartState, s.EndState, entities); err != nil {
			return err
		}
	}
	if err := applyOrderTx(ctx, tx, scriptID, orderedIDs); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// tempShotNo 把占位号推到一个不会与正常镜号相撞的区间。
//
// 镜号上限是 MaxShots（200），所以 -1000 以下永远安全。
func tempShotNo(seq int) int { return -1000 + seq }

func (r *Repo) DeleteShot(ctx context.Context, scriptID, shotID int64) error {
	tag, err := r.pool.Exec(ctx,
		`delete from shot where id = $1 and script_id = $2`, shotID, scriptID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) CountShots(ctx context.Context, scriptID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`select count(*) from shot where script_id = $1`, scriptID).Scan(&n)
	return n, err
}

// SavePromptInput 是一次提示词写入。
type SavePromptInput struct {
	ScriptID    int64
	ShotID      int64
	Kind        string
	Prompt      string
	Status      string
	Source      string
	Fingerprint string
	// ExpectedVersion 为 0 表示不做乐观锁（合成是服务端发起的，
	// 它按自己刚读到的上游算，没有"用户拿着旧版本覆盖"这回事）。
	ExpectedVersion int
}

// SavePrompt 写一条提示词。
//
// ⚠️ 图片与视频两套字段**分开写**：用户可能只重算其中一套，
// 一起写的话另一套会被空值冲掉。列名由 kind 决定，因此这里必须
// 显式分支 —— 用字符串拼列名会让这段代码变成一个注入点。
func (r *Repo) SavePrompt(ctx context.Context, in SavePromptInput) (Shot, error) {
	// ⚠️ 空字符串的语义是「这一项不动」，**不是「清空」** ——
	// 所以用 coalesce 而不是 nullif。合成失败时只想把状态标成 failed，
	// 用 nullif 的话会把用户手改过的那版词一起抹掉，他得从头再写一遍
	// （而他什么都没做错，只是点了一次重新生成）。
	var sql string
	switch in.Kind {
	case "image":
		sql = `update shot set
			image_prompt = coalesce(nullif($4,''), image_prompt),
			image_prompt_status = $5,
			image_prompt_source = coalesce(nullif($6,''), image_prompt_source),
			image_prompt_fingerprint = coalesce(nullif($7,''), image_prompt_fingerprint),
			version = version + 1, updated_at = now()
			where id = $1 and script_id = $2 and ($3 = 0 or version = $3)`
	case "video":
		sql = `update shot set
			video_prompt = coalesce(nullif($4,''), video_prompt),
			video_prompt_status = $5,
			video_prompt_source = coalesce(nullif($6,''), video_prompt_source),
			video_prompt_fingerprint = coalesce(nullif($7,''), video_prompt_fingerprint),
			version = version + 1, updated_at = now()
			where id = $1 and script_id = $2 and ($3 = 0 or version = $3)`
	default:
		return Shot{}, errors.New("未知的提示词种类: " + in.Kind)
	}
	tag, err := r.pool.Exec(ctx, sql, in.ShotID, in.ScriptID, in.ExpectedVersion,
		in.Prompt, in.Status, in.Source, in.Fingerprint)
	if err != nil {
		return Shot{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := r.GetShot(ctx, in.ScriptID, in.ShotID); err != nil {
			return Shot{}, err
		}
		return Shot{}, ErrVersionConflict
	}
	return r.GetShot(ctx, in.ScriptID, in.ShotID)
}

// MarkPromptsStale 把一个镜头的两套提示词标成 stale。
//
// ⚠️ 这是**即时反馈**，不是 stale 的正确性来源：漏掉任何一个调用点
// 都不会有编译错误。正确性靠行上的指纹（读的时候比一比），
// 见 domain.EffectiveStatus。
//
// 只动 ready 的那些：pending 还没合成过、failed 有自己的含义、
// stale 已经是 stale 了。
func (r *Repo) MarkPromptsStale(ctx context.Context, shotID int64) error {
	_, err := r.pool.Exec(ctx, `
		update shot set
			image_prompt_status = case when image_prompt_status = 'ready'
			                           then 'stale' else image_prompt_status end,
			video_prompt_status = case when video_prompt_status = 'ready'
			                           then 'stale' else video_prompt_status end,
			updated_at = now()
		where id = $1`, shotID)
	return err
}

// MarkScriptPromptsStale 把整个脚本的提示词标 stale（全片风格变了时用）。
func (r *Repo) MarkScriptPromptsStale(ctx context.Context, scriptID int64) error {
	_, err := r.pool.Exec(ctx, `
		update shot set
			image_prompt_status = case when image_prompt_status = 'ready'
			                           then 'stale' else image_prompt_status end,
			video_prompt_status = case when video_prompt_status = 'ready'
			                           then 'stale' else video_prompt_status end,
			updated_at = now()
		where script_id = $1`, scriptID)
	return err
}
