package repo

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 营销角色（平台预置）数据访问（MKT-T-34 用户端只读；管理端写入在 MKT-T-36 追加）。
//
// ⚠️ 预置角色单独一张表、没有 workspace 列（dev/05 MKD-30）：用户侧查询**永远不要**
// 写 `workspace_id in ($ws, 0)` 去把它混进「我的」—— 那样它会出现在可编辑列表里。
// ⚠️ 用户侧只认 `is_enabled = true`：停用与不存在回同一个 ErrNotFound（不给探测留缝）。

// PresetCharacter 是一行预置角色。
type PresetCharacter struct {
	ID            int64
	Name          string
	Summary       string
	ImageMediaIDs []int64
	Appearance    string
	VoiceText     string
	// VoiceMediaID 参考音频；0 = 没有。
	VoiceMediaID int64
	Sort         int
	IsEnabled    bool
	Version      int
	CreatedBy    int64
	UpdatedBy    int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const presetCols = `pc.id, pc.name, pc.summary, pc.image_media_ids, pc.appearance, pc.voice_text,
	coalesce(pc.voice_media_id, 0), pc.sort, pc.is_enabled, pc.version,
	coalesce(pc.created_by_admin_id, 0), coalesce(pc.updated_by_admin_id, 0), pc.created_at, pc.updated_at`

const presetFrom = ` from marketing_preset_character pc `

func scanPreset(row pgx.Row) (PresetCharacter, error) {
	var p PresetCharacter
	var raw []byte
	if err := row.Scan(&p.ID, &p.Name, &p.Summary, &raw, &p.Appearance, &p.VoiceText, &p.VoiceMediaID,
		&p.Sort, &p.IsEnabled, &p.Version, &p.CreatedBy, &p.UpdatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return PresetCharacter{}, mapNoRows(err)
	}
	ids, err := idsFromJSON(raw)
	if err != nil {
		return PresetCharacter{}, err
	}
	p.ImageMediaIDs = ids
	return p, nil
}

// PresetFilter 是用户端预置角色列表条件（固定按 sort, id 排序）。
type PresetFilter struct {
	Q      string
	Cursor string
	Limit  int
}

type presetCursor struct {
	S int   `json:"s"` // sort
	I int64 `json:"i"` // id
}

// EncodePresetCursor 生成下一页游标（排序键 sort + id）。
func EncodePresetCursor(p PresetCharacter) string {
	b, _ := json.Marshal(presetCursor{S: p.Sort, I: p.ID})
	return encodeCursor(b)
}

func decodePresetCursor(s string) (presetCursor, error) {
	b, err := decodeCursor(s)
	if err != nil {
		return presetCursor{}, ErrBadCursor
	}
	var c presetCursor
	if err := json.Unmarshal(b, &c); err != nil || c.I <= 0 {
		return presetCursor{}, ErrBadCursor
	}
	return c, nil
}

// ListEnabledPresets 列出**启用**的预置角色，按 `sort, id`（与管理端排好的顺序一致）。
// 返回 (行, 是否还有下一页)。
func (r *Repo) ListEnabledPresets(ctx context.Context, f PresetFilter) ([]PresetCharacter, bool, error) {
	if f.Limit <= 0 {
		f.Limit = 20
	}
	args := []any{}
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	cond := []string{"pc.is_enabled = true"}
	if q := strings.TrimSpace(f.Q); q != "" {
		cond = append(cond, "pc.name ilike '%' || "+add(escapeLike(q))+" || '%' escape '\\'")
	}
	if f.Cursor != "" {
		c, err := decodePresetCursor(f.Cursor)
		if err != nil {
			return nil, false, err
		}
		cond = append(cond, "(pc.sort, pc.id) > ("+add(c.S)+"::int, "+add(c.I)+"::bigint)")
	}
	sql := `select ` + presetCols + presetFrom + ` where ` + strings.Join(cond, " and ") +
		` order by pc.sort asc, pc.id asc limit ` + add(f.Limit+1)
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []PresetCharacter{}
	for rows.Next() {
		p, err := scanPreset(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, p)
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

// GetEnabledPreset 取一个**启用**的预置角色；停用 / 不存在 → ErrNotFound（同一个回答）。
func (r *Repo) GetEnabledPreset(ctx context.Context, id int64) (PresetCharacter, error) {
	return scanPreset(r.pool.QueryRow(ctx, `select `+presetCols+presetFrom+`
		where pc.id = $1 and pc.is_enabled = true`, id))
}
