package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 工作区角色库数据访问（MKT-T-34；dev/05 §2.2、MKD-30、MKD-31、MKD-34、MKD-36、MKD-37）。
//
// ⚠️ 用户侧每条 SQL 都带 `workspace_id = $n and deleted_at is null`（越权防线落点，CD-04）。
// 唯一的例外是 CharacterMediaReferences：媒体引用检查器按定义**跨工作区**查。
// ⚠️ 预置角色在另一张表（preset.go）：这里**永远不要**写 `workspace_id in ($ws, 0)`。
// ⚠️ image_media_ids 存字符串数组（与商品同形，见 product.go 的注释）。

// ErrPresetCopyExists 是同一 (工作区, 预置角色, 版本) 已有一份未删除的副本（uk_marketing_character_preset）。
var ErrPresetCopyExists = errors.New("该营销角色的这一版已添加过")

// 角色来源（ck_marketing_character_source）。
const (
	CharacterSourceUser   = "user"
	CharacterSourcePreset = "preset"
)

// Character 是一行工作区角色。
type Character struct {
	ID            int64
	WorkspaceID   int64
	Name          string
	ImageMediaIDs []int64
	Appearance    string
	StyleCode     string
	VoiceText     string
	// VoiceMediaID 参考音频；0 = 没有。
	VoiceMediaID int64
	Source       string
	// SourcePresetID / SourcePresetVersion 只在 source=preset 时非 0。
	SourcePresetID      int64
	SourcePresetVersion int
	Version             int
	CreatedBy           int64
	UpdatedBy           int64
	// UpdatedByName 最后编辑者昵称（冲突提示「是谁改的」，CD-58）；为空 = 未知。
	UpdatedByName string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CharacterFilter 是列表条件（排序取值与商品相同）。
type CharacterFilter struct {
	WorkspaceID int64
	Q           string
	Sort        ProductSort
	Cursor      string
	Limit       int
}

const characterCols = `c.id, c.workspace_id, c.name, c.image_media_ids, c.appearance, coalesce(c.style_code, ''),
	c.voice_text, coalesce(c.voice_media_id, 0), c.source, coalesce(c.source_preset_id, 0),
	coalesce(c.source_preset_version, 0), c.version, coalesce(c.created_by_user_id, 0),
	coalesce(c.updated_by_user_id, 0), coalesce(u.nickname, ''), c.created_at, c.updated_at`

// ⚠️ `left join`：最后编辑者账号已清理时，内连接会让整行消失。
const characterFrom = ` from marketing_character c left join app_user u on u.id = c.updated_by_user_id `

func scanCharacter(row pgx.Row) (Character, error) {
	var c Character
	var raw []byte
	if err := row.Scan(&c.ID, &c.WorkspaceID, &c.Name, &raw, &c.Appearance, &c.StyleCode, &c.VoiceText,
		&c.VoiceMediaID, &c.Source, &c.SourcePresetID, &c.SourcePresetVersion, &c.Version, &c.CreatedBy,
		&c.UpdatedBy, &c.UpdatedByName, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Character{}, mapNoRows(err)
	}
	ids, err := idsFromJSON(raw)
	if err != nil {
		return Character{}, err
	}
	c.ImageMediaIDs = ids
	return c, nil
}

// mapCharacterUnique 把两条部分唯一索引分开：重名是用户要改名字，副本已存在是 adopt 要回那一份。
func mapCharacterUnique(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "uk_marketing_character_preset" {
			return ErrPresetCopyExists
		}
		return ErrDuplicate
	}
	return err
}

// EncodeCharacterCursor 生成下一页游标（形状与商品游标相同，与排序绑定）。
func EncodeCharacterCursor(sort ProductSort, c Character) string {
	cur := productCursor{S: string(sort), I: c.ID}
	switch sort {
	case ProductSortCreated:
		cur.K = c.CreatedAt.UTC().Format(time.RFC3339Nano)
	case ProductSortName:
		cur.K = strings.ToLower(c.Name)
	default:
		cur.K = c.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	b, _ := json.Marshal(cur)
	return encodeCursor(b)
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// InsertCharacter 新建角色。配额满 → ErrQuota；同工作区重名 → ErrDuplicate；
// 同一预置版本已有副本 → ErrPresetCopyExists。
//
// ⚠️ 计数与插入在同一事务、先拿工作区级 advisory 锁（MKD-34、MKR-19），同商品。
func (r *Repo) InsertCharacter(ctx context.Context, c Character, max int) (Character, error) {
	imgs, err := idsToJSON(c.ImageMediaIDs)
	if err != nil {
		return Character{}, err
	}
	source := c.Source
	if source == "" {
		source = CharacterSourceUser
	}
	var presetVer any
	if c.SourcePresetVersion > 0 {
		presetVer = c.SourcePresetVersion
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Character{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`select pg_advisory_xact_lock(hashtextextended('marketing_character:' || $1::bigint::text, 0))`,
		c.WorkspaceID); err != nil {
		return Character{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, `select count(*) from marketing_character
		where workspace_id = $1 and deleted_at is null`, c.WorkspaceID).Scan(&n); err != nil {
		return Character{}, err
	}
	if n >= max {
		return Character{}, ErrQuota
	}
	if _, err := tx.Exec(ctx, `
		insert into marketing_character (id, workspace_id, name, image_media_ids, appearance, style_code,
		                                 voice_text, voice_media_id, source, source_preset_id,
		                                 source_preset_version, created_by_user_id, updated_by_user_id)
		values ($1, $2, $3, $4::jsonb, $5, nullif($6, ''), $7, $8, $9, $10, $11,
		        nullif($12::bigint, 0), nullif($12::bigint, 0))`,
		c.ID, c.WorkspaceID, c.Name, string(imgs), c.Appearance, c.StyleCode, c.VoiceText,
		nullIfZero(c.VoiceMediaID), source, nullIfZero(c.SourcePresetID), presetVer, c.CreatedBy); err != nil {
		return Character{}, mapCharacterUnique(err)
	}
	out, err := scanCharacter(tx.QueryRow(ctx, `select `+characterCols+characterFrom+`
		where c.id = $1 and c.workspace_id = $2 and c.deleted_at is null`, c.ID, c.WorkspaceID))
	if err != nil {
		return Character{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Character{}, err
	}
	return out, nil
}

// GetCharacter 取本工作区未删除的角色。
func (r *Repo) GetCharacter(ctx context.Context, workspaceID, id int64) (Character, error) {
	return scanCharacter(r.pool.QueryRow(ctx, `select `+characterCols+characterFrom+`
		where c.id = $1 and c.workspace_id = $2 and c.deleted_at is null`, id, workspaceID))
}

// GetCharacterByPreset 取本工作区里某预置角色某一版的未删除副本（adopt 幂等用）。
func (r *Repo) GetCharacterByPreset(ctx context.Context, workspaceID, presetID int64, version int) (Character, error) {
	return scanCharacter(r.pool.QueryRow(ctx, `select `+characterCols+characterFrom+`
		where c.workspace_id = $1 and c.source_preset_id = $2 and c.source_preset_version = $3
		  and c.deleted_at is null`, workspaceID, presetID, version))
}

// UpdateCharacter 整份更新内容（乐观锁）。**不改** source / source_preset_*（来源是事实不是内容）。
// 版本不匹配 → ErrStale；不存在 / 他人工作区 / 已删除 → ErrNotFound；重名 → ErrDuplicate。
func (r *Repo) UpdateCharacter(ctx context.Context, c Character, version int) (Character, error) {
	imgs, err := idsToJSON(c.ImageMediaIDs)
	if err != nil {
		return Character{}, err
	}
	tag, err := r.pool.Exec(ctx, `
		update marketing_character
		   set name = $4, image_media_ids = $5::jsonb, appearance = $6, style_code = nullif($7, ''),
		       voice_text = $8, voice_media_id = $9, version = version + 1,
		       updated_by_user_id = nullif($10::bigint, 0), updated_at = now()
		 where id = $1 and workspace_id = $2 and deleted_at is null and version = $3`,
		c.ID, c.WorkspaceID, version, c.Name, string(imgs), c.Appearance, c.StyleCode, c.VoiceText,
		nullIfZero(c.VoiceMediaID), c.UpdatedBy)
	if err != nil {
		return Character{}, mapCharacterUnique(err)
	}
	if tag.RowsAffected() == 0 {
		if _, gerr := r.GetCharacter(ctx, c.WorkspaceID, c.ID); gerr != nil {
			return Character{}, gerr
		}
		return Character{}, ErrStale
	}
	return r.GetCharacter(ctx, c.WorkspaceID, c.ID)
}

// SoftDeleteCharacter 软删，返回删除前的行（调用方据此尽力删媒体，MKD-31）。重复删除 → ErrNotFound。
func (r *Repo) SoftDeleteCharacter(ctx context.Context, workspaceID, id, userID int64) (Character, error) {
	var raw []byte
	c := Character{ID: id, WorkspaceID: workspaceID}
	err := r.pool.QueryRow(ctx, `
		update marketing_character
		   set deleted_at = now(), updated_by_user_id = nullif($3::bigint, 0), updated_at = now()
		 where id = $1 and workspace_id = $2 and deleted_at is null
		returning name, image_media_ids, coalesce(voice_media_id, 0)`, id, workspaceID, userID).
		Scan(&c.Name, &raw, &c.VoiceMediaID)
	if err != nil {
		return Character{}, mapNoRows(err)
	}
	ids, err := idsFromJSON(raw)
	if err != nil {
		return Character{}, err
	}
	c.ImageMediaIDs = ids
	return c, nil
}

// ListCharacters 列出本工作区未删除角色（服务端搜索 + 游标分页，MKD-37）。返回 (行, 是否还有下一页)。
func (r *Repo) ListCharacters(ctx context.Context, f CharacterFilter) ([]Character, bool, error) {
	if !f.Sort.Valid() {
		f.Sort = ProductSortUpdated
	}
	if f.Limit <= 0 {
		f.Limit = 20
	}
	args := []any{f.WorkspaceID}
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	cond := []string{"c.workspace_id = $1", "c.deleted_at is null"}
	if q := strings.TrimSpace(f.Q); q != "" {
		cond = append(cond, "c.name ilike '%' || "+add(escapeLike(q))+" || '%' escape '\\'")
	}
	var order string
	switch f.Sort {
	case ProductSortCreated:
		order = "c.created_at desc, c.id desc"
	case ProductSortName:
		order = "lower(c.name) asc, c.id asc"
	default:
		order = "c.updated_at desc, c.id desc"
	}
	if f.Cursor != "" {
		cur, err := decodeProductCursor(f.Sort, f.Cursor)
		if err != nil {
			return nil, false, err
		}
		switch f.Sort {
		case ProductSortCreated:
			cond = append(cond, "(c.created_at, c.id) < ("+add(cur.K)+"::timestamptz, "+add(cur.I)+")")
		case ProductSortName:
			cond = append(cond, "(lower(c.name), c.id) > ("+add(cur.K)+", "+add(cur.I)+")")
		default:
			cond = append(cond, "(c.updated_at, c.id) < ("+add(cur.K)+"::timestamptz, "+add(cur.I)+")")
		}
	}
	sql := `select ` + characterCols + characterFrom + ` where ` + strings.Join(cond, " and ") +
		` order by ` + order + ` limit ` + add(f.Limit+1)
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []Character{}
	for rows.Next() {
		c, err := scanCharacter(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > f.Limit
	if more {
		out = out[:f.Limit]
	}
	return out, more, nil
}

// CharacterMediaReferences 返回引用该媒体（图片或参考音频）的**未删除**角色 ID（MKT-D-53）。
//
// ⚠️ **跨工作区查**（同商品检查器）；只认未删除的（MKD-31）。两个条件分别命中
// idx_marketing_character_media（GIN）与 idx_marketing_character_voice。
func (r *Repo) CharacterMediaReferences(ctx context.Context, mediaID int64) ([]int64, error) {
	ref, err := idsToJSON([]int64{mediaID})
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		select id from marketing_character where image_media_ids @> $1::jsonb and deleted_at is null
		union
		select id from marketing_character where voice_media_id = $2 and deleted_at is null
		order by id`, string(ref), mediaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
