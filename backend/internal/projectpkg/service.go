package projectpkg

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store 是对象存储里本包要用的部分（objstore.Client 满足）。
type Store interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	Delete(ctx context.Context, key string) error
	Bucket() string
}

// Owner 是导入后各行归属的本机主体（单机版只有一个）。
type Owner struct {
	WorkspaceID, UserID, PayerID int64
}

// Service 导出与导入项目。
type Service struct {
	pool       *pgxpool.Pool
	store      Store
	nextID     func() int64
	appVersion string
	now        func() time.Time
}

// New 创建 Service。
func New(pool *pgxpool.Pool, store Store, nextID func() int64, appVersion string) *Service {
	return &Service{pool: pool, store: store, nextID: nextID, appVersion: appVersion, now: time.Now}
}

// ErrNotFound 项目不存在（或不属于本工作区）。
var ErrNotFound = errors.New("项目不存在")

// BadPackageError 是「包本身有问题」的错误：调用方回 400 并原样给出原因。
type BadPackageError struct{ Reason string }

func (e *BadPackageError) Error() string { return e.Reason }

func bad(format string, args ...any) error {
	return &BadPackageError{Reason: fmt.Sprintf(format, args...)}
}

// table 是一张随项目导出的从属表。where 里 $1 = 项目 ID。
//
// ⚠️ 顺序就是导入时的插入顺序（没有外键，顺序只为了读起来自然）。
// 不在这里的、刻意不导的：agent_session / canvas_op_log（会话与操作日志，可能含上游原文与地址）、
// *_llm_call 与 render_job（调用与任务记录）、timeline_snapshot（撤销历史）、workflow_run（运行记录）、
// library_item（工作区级素材库，不属于某个项目）。
type table struct{ name, where string }

var tables = []table{
	{"project_document", `t.project_id = $1 and t.status = 'active'`},
	{"upload_reference", `t.project_id = $1 and t.status = 'active'`},
	{"generation", `t.project_id = $1`},
	{"generation_output", `t.generation_id in (select id from generation where project_id = $1)`},
	{"script_instance", `t.project_id = $1`},
	{"shot", `t.script_id in (select id from script_instance where project_id = $1)`},
	{"shot_asset_ref", `t.shot_id in (select s.id from shot s join script_instance si on si.id = s.script_id where si.project_id = $1)`},
	// 资产是工作区级的：只带本项目引用到的（镜头引用、组子项、生成记录），导入后是新的一份。
	{"script_asset", `t.id in (
		select r.script_asset_id from shot_asset_ref r join shot s on s.id = r.shot_id
		  join script_instance si on si.id = s.script_id where si.project_id = $1
		union select gi.script_asset_id from group_item gi join group_node gn on gn.id = gi.group_node_id
		  where gn.project_id = $1 and gi.script_asset_id is not null
		union select g.script_asset_id from generation g where g.project_id = $1 and g.script_asset_id is not null)`},
	{"group_node", `t.project_id = $1`},
	{"group_item", `t.group_node_id in (select id from group_node where project_id = $1)`},
	{"timeline", `t.project_id = $1`},
	{"timeline_track", `t.timeline_id in (select id from timeline where project_id = $1)`},
	{"timeline_clip", `t.track_id in (select tr.id from timeline_track tr join timeline tl on tl.id = tr.timeline_id where tl.project_id = $1)`},
	{"timeline_subtitle", `t.timeline_id in (select id from timeline where project_id = $1)`},
	{"timeline_text", `t.timeline_id in (select id from timeline where project_id = $1)`},
	{"director_scene", `t.project_id = $1`},
	{"director_media", `t.project_id = $1`},
}

// scrub 是导出前清掉的列：上游原文与可能带地址的字段（TT-F-10「不含上游调用日志」）。
var scrub = map[string][]string{
	"generation": {"provider_raw_result", "error_detail", "provider_task_id"},
}

// projectFile 是 project.json。
type projectFile struct {
	Project    map[string]any  `json:"project"`
	CanvasData json.RawMessage `json:"canvasData"`
}

// ---------------------------------------------------------------- 导出

// Export 把项目写成 zip 临时文件，返回文件路径与建议文件名。调用方负责删除临时文件。
func (s *Service) Export(ctx context.Context, workspaceID, projectID int64) (string, string, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var projRaw, canvas []byte
	err = tx.QueryRow(ctx, `select to_jsonb(p) - 'canvas_data', p.canvas_data::text from project p
		where p.id = $1 and p.workspace_id = $2 and p.status = 'active'`, projectID, workspaceID).Scan(&projRaw, &canvas)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	project, err := decodeRow(projRaw)
	if err != nil {
		return "", "", err
	}
	delete(project, "create_idempotency_key")

	m := Manifest{Format: Format, SchemaVersion: SchemaVersion, AppVersion: s.appVersion,
		ExportedAt: s.now().UTC(), ProjectName: fmt.Sprint(project["name"]), Tables: map[string]int{}}
	data := map[string][]map[string]any{}
	candidates := map[string]struct{}{}
	if v, err := Decode(canvas); err == nil {
		CollectIDs(v, candidates)
	}
	CollectIDs(map[string]any(project), candidates)
	for _, t := range tables {
		rows, err := queryRows(ctx, tx, `select to_jsonb(t) from `+t.name+` t where `+t.where, projectID)
		if err != nil {
			return "", "", fmt.Errorf("读取 %s：%w", t.name, err)
		}
		for _, r := range rows {
			for _, col := range scrub[t.name] {
				delete(r, col)
			}
			CollectIDs(map[string]any(r), candidates)
		}
		data[t.name] = rows
		m.Tables[t.name] = len(rows)
	}

	// 生成记录里的模型与操作按 code 记（两台机器上 ID 不同）。
	m.Catalog, err = catalogCodes(ctx, tx, data["generation"])
	if err != nil {
		return "", "", err
	}

	// 媒体 = 所有可能是 ID 的值里，确实是本工作区有效媒体的那些；再补上它们的派生源（存在才带）。
	media, err := s.mediaRows(ctx, tx, workspaceID, candidates)
	if err != nil {
		return "", "", err
	}

	f, err := os.CreateTemp("", "tabtv-export-*.zip")
	if err != nil {
		return "", "", err
	}
	name := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(name)
		}
	}()
	zw := zip.NewWriter(f)
	var kept []map[string]any
	for _, row := range media {
		id := fmt.Sprint(row["id"])
		key, _ := row["object_key"].(string)
		ct, _ := row["content_type"].(string)
		rc, err := s.store.Open(ctx, key)
		if err != nil {
			// 源媒体已经没了：不中断，记进 warnings，导入后对应节点显示缺失（而不是静默裂图）。
			m.Warnings = append(m.Warnings, fmt.Sprintf("媒体 %s 的文件取不到，未打进包：%v", id, err))
			continue
		}
		// 媒体本身多半已压缩（图片 / 视频），用 Store 不再压一遍。
		w, err := zw.CreateHeader(&zip.FileHeader{Name: MediaEntry(id, mediaExt(key, ct)), Method: zip.Store, Modified: m.ExportedAt})
		if err == nil {
			_, err = io.Copy(w, rc)
		}
		_ = rc.Close()
		if err != nil {
			return "", "", fmt.Errorf("写入媒体 %s：%w", id, err)
		}
		row["_entry"] = MediaEntry(id, mediaExt(key, ct))
		delete(row, "object_key")
		delete(row, "bucket")
		kept = append(kept, row)
	}
	m.Media = len(kept)

	put := func(name string, v any) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: m.ExportedAt})
		if err != nil {
			return err
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if err := put("manifest.json", m); err != nil {
		return "", "", err
	}
	if err := put("project.json", projectFile{Project: project, CanvasData: canvas}); err != nil {
		return "", "", err
	}
	if err := put("media.json", kept); err != nil {
		return "", "", err
	}
	names := make([]string, 0, len(data))
	for n := range data {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := put("data/"+n+".json", data[n]); err != nil {
			return "", "", err
		}
	}
	// ⚠️ zip 的中央目录在 Close 时才写：不检查的话会交出一个字节数正常却解不开的包（CD-47）。
	if err := zw.Close(); err != nil {
		return "", "", err
	}
	if err := f.Close(); err != nil {
		return "", "", err
	}
	ok = true
	return name, FileName(m.ProjectName), nil
}

func (s *Service) mediaRows(ctx context.Context, tx pgx.Tx, workspaceID int64, candidates map[string]struct{}) ([]map[string]any, error) {
	var ids []int64
	for c := range candidates {
		if n, err := strconv.ParseInt(c, 10, 64); err == nil && n > 0 {
			ids = append(ids, n)
		}
	}
	seen := map[string]bool{}
	var out []map[string]any
	for round := 0; round < 3 && len(ids) > 0; round++ {
		rows, err := queryRows(ctx, tx, `select to_jsonb(m) from media_object m
			where m.id = any($1) and m.workspace_id = $2 and m.status = 'active'`, ids, workspaceID)
		if err != nil {
			return nil, err
		}
		ids = nil
		for _, r := range rows {
			id := fmt.Sprint(r["id"])
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, r)
			if src := r["source_media_id"]; src != nil {
				if n, err := strconv.ParseInt(fmt.Sprint(src), 10, 64); err == nil && !seen[fmt.Sprint(src)] {
					ids = append(ids, n)
				}
			}
		}
	}
	return out, nil
}

func catalogCodes(ctx context.Context, tx pgx.Tx, gens []map[string]any) (CatalogCodes, error) {
	c := CatalogCodes{Operations: map[string]string{}, Models: map[string]string{}}
	lookup := func(table, col string, into map[string]string) error {
		var ids []int64
		for _, g := range gens {
			if n, err := strconv.ParseInt(fmt.Sprint(g[col]), 10, 64); err == nil {
				ids = append(ids, n)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		rows, err := tx.Query(ctx, `select id, code from `+table+` where id = any($1)`, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var code string
			if err := rows.Scan(&id, &code); err != nil {
				return err
			}
			into[strconv.FormatInt(id, 10)] = code
		}
		return rows.Err()
	}
	if err := lookup("operation", "operation_id", c.Operations); err != nil {
		return c, err
	}
	return c, lookup("model", "model_id", c.Models)
}

func mediaExt(key, contentType string) string {
	if e := path.Ext(key); e != "" && len(e) <= 6 {
		return strings.ToLower(e)
	}
	switch contentType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "audio/mpeg":
		return ".mp3"
	}
	return ".bin"
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func queryRows(ctx context.Context, q querier, sql string, args ...any) ([]map[string]any, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		r, err := decodeRow(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func decodeRow(raw []byte) (map[string]any, error) {
	v, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("行不是 JSON 对象")
	}
	return m, nil
}
