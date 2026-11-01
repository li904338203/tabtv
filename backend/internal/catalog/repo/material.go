package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
)

// MaterialRow 是素材的查询结果，含当前用户是否已收藏。
type MaterialRow struct {
	domain.Material
	IsFavorited bool
}

// ListMaterials 按类型列出素材。
// userID 为 0 时不带收藏标记（管理端场景）。
func (r *Repo) ListMaterials(ctx context.Context, mt domain.MaterialType, userID int64,
	keyword, category string, commercialOnly, favoritedOnly, enabledOnly bool) ([]MaterialRow, error) {

	table := mt.Table()
	// 表名来自受控枚举而非用户输入，不存在注入面。
	q := `
		select m.code, m.name, coalesce(m.category,''), coalesce(m.prompt,''),
		       coalesce(m.author,''), m.is_commercial, m.cover_media_id, m.sort, m.is_enabled,
		       (f.id is not null) as favorited
		from ` + table + ` m
		left join material_favorite f
		       on f.material_type = $1 and f.material_id = m.id and f.user_id = $2
		where (not $3 or m.is_enabled)
		  and ($4 = '' or m.name ilike '%'||$4||'%' or m.code ilike '%'||$4||'%')
		  and ($5 = '' or m.category = $5)
		  and (not $6 or m.is_commercial)
		  and (not $7 or f.id is not null)
		order by m.sort, m.code`

	rows, err := r.pool.Query(ctx, q, string(mt), userID, enabledOnly,
		keyword, category, commercialOnly, favoritedOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MaterialRow
	for rows.Next() {
		var m MaterialRow
		m.Type = mt
		if err := rows.Scan(&m.Code, &m.Name, &m.Category, &m.Prompt, &m.Author,
			&m.IsCommercial, &m.CoverMediaID, &m.Sort, &m.IsEnabled, &m.IsFavorited); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Repo) SaveMaterial(ctx context.Context, id int64, m domain.Material) error {
	table := m.Type.Table()
	_, err := r.pool.Exec(ctx, `
		insert into `+table+` (id, code, name, category, prompt, author,
		  is_commercial, cover_media_id, sort, is_enabled)
		values ($1,$2,$3,nullif($4,''),nullif($5,''),nullif($6,''),$7,$8,$9,$10)
		on conflict (code) do update
		set name=excluded.name, category=excluded.category, prompt=excluded.prompt,
		    author=excluded.author, is_commercial=excluded.is_commercial,
		    cover_media_id=excluded.cover_media_id, sort=excluded.sort,
		    is_enabled=excluded.is_enabled, updated_at=now()`,
		id, m.Code, m.Name, m.Category, m.Prompt, m.Author,
		m.IsCommercial, m.CoverMediaID, m.Sort, m.IsEnabled)
	return err
}

// MaterialPrompts 取一组素材的提示词，供服务端拼接。
// MaterialPromptsByCode 返回 code → 提示词（只含启用中的）。
//
// ⚠️ 与 MaterialPrompts 的区别是**能看出哪些 code 查不到**：
// 用户选了一个已下线或拼错的素材码，静默丢掉就是"选了等于没选"——
// 生成出来没有那个效果，而界面上它明明是选中的。
func (r *Repo) MaterialPromptsByCode(ctx context.Context, mt domain.MaterialType,
	codes []string) (map[string]string, error) {
	out := map[string]string{}
	if len(codes) == 0 {
		return out, nil
	}
	table := mt.Table()
	rows, err := r.pool.Query(ctx,
		`select code, coalesce(prompt,'') from `+table+`
		 where code = any($1) and is_enabled order by sort, code`, codes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var code, prompt string
		if err := rows.Scan(&code, &prompt); err != nil {
			return nil, err
		}
		out[code] = prompt
	}
	return out, rows.Err()
}

func (r *Repo) MaterialPrompts(ctx context.Context, mt domain.MaterialType, codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	table := mt.Table()
	rows, err := r.pool.Query(ctx,
		`select coalesce(prompt,'') from `+table+`
		 where code = any($1) and is_enabled order by sort, code`, codes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ToggleFavorite 切换收藏。返回切换后的状态。
//
// 三张 *_favorite 已合并为一张 material_favorite（数据模型 §12），
// 收藏列表因此不必 union 三次。
func (r *Repo) ToggleFavorite(ctx context.Context, id, userID int64,
	mt domain.MaterialType, code string, favorite bool) (bool, error) {

	table := mt.Table()
	var materialID int64
	err := r.pool.QueryRow(ctx, `select id from `+table+` where code = $1`, code).Scan(&materialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}

	if favorite {
		_, err = r.pool.Exec(ctx, `
			insert into material_favorite (id, user_id, material_type, material_id)
			values ($1,$2,$3,$4)
			on conflict (user_id, material_type, material_id) do nothing`,
			id, userID, string(mt), materialID)
		return true, err
	}
	_, err = r.pool.Exec(ctx, `
		delete from material_favorite
		where user_id=$1 and material_type=$2 and material_id=$3`,
		userID, string(mt), materialID)
	return false, err
}

type VoiceRow struct {
	domain.Voice
	IsFavorited bool
}

func (r *Repo) ListVoices(ctx context.Context, userID int64,
	keyword, language, gender string, favoritedOnly, enabledOnly bool) ([]VoiceRow, error) {

	rows, err := r.pool.Query(ctx, `
		select v.code, v.name, coalesce(v.upstream_voice_id,''), coalesce(v.language,''),
		       coalesce(v.gender,''), v.icon_media_id, v.preview_media_id,
		       v.sort, v.is_enabled, (f.id is not null)
		from voice v
		left join material_favorite f
		       on f.material_type = 'voice' and f.material_id = v.id and f.user_id = $1
		where (not $2 or v.is_enabled)
		  and ($3 = '' or v.name ilike '%'||$3||'%' or v.code ilike '%'||$3||'%')
		  and ($4 = '' or v.language = $4)
		  and ($5 = '' or v.gender = $5)
		  and (not $6 or f.id is not null)
		order by v.sort, v.code`,
		userID, enabledOnly, keyword, language, gender, favoritedOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VoiceRow
	for rows.Next() {
		var v VoiceRow
		if err := rows.Scan(&v.Code, &v.Name, &v.UpstreamVoiceID, &v.Language,
			&v.Gender, &v.IconMediaID, &v.PreviewMediaID, &v.Sort,
			&v.IsEnabled, &v.IsFavorited); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repo) SaveVoice(ctx context.Context, id int64, v domain.Voice) error {
	_, err := r.pool.Exec(ctx, `
		insert into voice (id, code, name, upstream_voice_id, language, gender,
		  icon_media_id, preview_media_id, sort, is_enabled)
		values ($1,$2,$3,nullif($4,''),nullif($5,''),nullif($6,''),$7,$8,$9,$10)
		on conflict (code) do update
		set name=excluded.name, upstream_voice_id=excluded.upstream_voice_id,
		    language=excluded.language, gender=excluded.gender,
		    icon_media_id=excluded.icon_media_id, preview_media_id=excluded.preview_media_id,
		    sort=excluded.sort, is_enabled=excluded.is_enabled, updated_at=now()`,
		id, v.Code, v.Name, v.UpstreamVoiceID, v.Language, v.Gender,
		v.IconMediaID, v.PreviewMediaID, v.Sort, v.IsEnabled)
	return err
}

func (r *Repo) GetVoicePreview(ctx context.Context, code string) (int64, error) {
	var id *int64
	err := r.pool.QueryRow(ctx,
		`select preview_media_id from voice where code = $1 and is_enabled`, code).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && id == nil) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return *id, nil
}
