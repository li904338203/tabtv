package projectpkg

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ImportResult 是一次导入的结果。
type ImportResult struct {
	ProjectID int64    `json:"projectId,string"`
	Name      string   `json:"name"`
	Warnings  []string `json:"warnings"`
}

// maxJSONBytes 是单个 JSON 条目的上限（画布与表数据），防一个伪造的超大 JSON 把内存撑爆。
const maxJSONBytes = int64(512) << 20

// Import 从 zip 文件导入为一个新项目。全部重新发号，不碰任何已有数据；失败时库里不留行、存储里不留文件。
func (s *Service) Import(ctx context.Context, owner Owner, zipPath string) (ImportResult, error) {
	var res ImportResult
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return res, bad("不是有效的 zip 文件：%v", err)
	}
	defer zr.Close()

	files, err := indexEntries(zr.File)
	if err != nil {
		return res, err
	}
	var m Manifest
	if err := readJSON(files, "manifest.json", &m); err != nil {
		return res, err
	}
	if err := CheckManifest(m); err != nil {
		return res, &BadPackageError{Reason: err.Error()}
	}
	var pf projectFile
	if err := readJSON(files, "project.json", &pf); err != nil {
		return res, err
	}
	if pf.Project == nil || pf.Project["id"] == nil {
		return res, bad("project.json 缺少项目信息")
	}
	var media []map[string]any
	if _, ok := files["media.json"]; ok {
		if err := readJSON(files, "media.json", &media); err != nil {
			return res, err
		}
	}
	data := map[string][]map[string]any{}
	for _, t := range tables {
		name := "data/" + t.name + ".json"
		if _, ok := files[name]; !ok {
			continue
		}
		var rows []map[string]any
		if err := readJSON(files, name, &rows); err != nil {
			return res, err
		}
		data[t.name] = rows
	}
	res.Warnings = append(res.Warnings, m.Warnings...)

	// ---- 重新发号：包里每一行的 ID 都换新
	ids := map[string]string{}
	assign := func(row map[string]any) {
		if row == nil || row["id"] == nil {
			return
		}
		old := fmt.Sprint(row["id"])
		if _, ok := ids[old]; !ok {
			ids[old] = strconv.FormatInt(s.nextID(), 10)
		}
	}
	assign(pf.Project)
	for _, rows := range data {
		for _, r := range rows {
			assign(r)
		}
	}
	for _, r := range media {
		assign(r)
	}
	ops, models, warns, err := s.localCatalog(ctx, m.Catalog)
	if err != nil {
		return res, err
	}
	res.Warnings = append(res.Warnings, warns...)

	// ---- 媒体先写存储（事务外：对象存储不在事务里），失败时把写过的删掉
	var written []string
	cleanup := func() {
		for _, k := range written {
			_ = s.store.Delete(context.WithoutCancel(ctx), k)
		}
	}
	var mediaRows []map[string]any
	for _, r := range media {
		entry, _ := r["_entry"].(string)
		f, ok := files[entry]
		if entry == "" || !ok || !strings.HasPrefix(entry, "media/") {
			res.Warnings = append(res.Warnings, fmt.Sprintf("媒体 %v 在包里没有文件，已跳过", r["id"]))
			continue
		}
		row := Remap(r, ids).(map[string]any)
		newID := fmt.Sprint(row["id"])
		purpose, _ := row["purpose"].(string)
		if purpose == "" {
			purpose = "upload_reference"
		}
		ct, _ := row["content_type"].(string)
		key := path.Join(purpose, s.now().UTC().Format("2006/01/02"), newID+path.Ext(entry))
		if err := s.putEntry(ctx, f, key, ct); err != nil {
			cleanup()
			return res, err
		}
		written = append(written, key)
		delete(row, "_entry")
		// 派生源没打进包时置空：source_media_id 只是溯源线索，留个指向别台机器的 ID 没有意义。
		if src := r["source_media_id"]; src != nil {
			if _, ok := ids[fmt.Sprint(src)]; !ok {
				row["source_media_id"] = nil
			}
		}
		row["bucket"] = s.store.Bucket()
		row["object_key"] = key
		row["size_bytes"] = json.Number(strconv.FormatUint(f.UncompressedSize64, 10))
		row["status"] = "active"
		row["deleted_at"] = nil
		mediaRows = append(mediaRows, row)
	}

	pid, name, err := s.insertAll(ctx, owner, pf, data, mediaRows, ids, ops, models)
	if err != nil {
		cleanup()
		return res, err
	}
	res.ProjectID, res.Name = pid, name
	if res.Warnings == nil {
		res.Warnings = []string{}
	}
	return res, nil
}

func (s *Service) insertAll(ctx context.Context, owner Owner, pf projectFile, data map[string][]map[string]any,
	mediaRows []map[string]any, ids, ops, models map[string]string) (int64, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	cols := map[string]map[string]bool{}
	columnsOf := func(t string) (map[string]bool, error) {
		if c, ok := cols[t]; ok {
			return c, nil
		}
		rows, err := tx.Query(ctx, `select column_name from information_schema.columns
			where table_schema = current_schema() and table_name = $1`, t)
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return nil, err
			}
			set[c] = true
		}
		rows.Close()
		cols[t] = set
		return set, rows.Err()
	}
	insert := func(t string, row map[string]any) error {
		set, err := columnsOf(t)
		if err != nil {
			return err
		}
		var names []string
		for k := range row {
			if set[k] { // 本机表上没有的列（包来自更新的版本）丢掉；本机多出的列走默认值
				names = append(names, pgx.Identifier{k}.Sanitize())
			}
		}
		if len(names) == 0 {
			return nil
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return err
		}
		list := strings.Join(names, ", ")
		_, err = tx.Exec(ctx, `insert into `+pgx.Identifier{t}.Sanitize()+` (`+list+`) select `+list+
			` from jsonb_populate_record(null::`+pgx.Identifier{t}.Sanitize()+`, $1::jsonb)`, raw)
		if err != nil {
			return fmt.Errorf("导入 %s 失败：%w", t, err)
		}
		return nil
	}

	for _, r := range mediaRows {
		own(r, owner)
		if err := insert("media_object", r); err != nil {
			return 0, "", err
		}
	}

	// ---- 项目：重名追加「（导入）」，version 从 1 起，不进任何文件夹
	proj := Remap(pf.Project, ids).(map[string]any)
	own(proj, owner)
	pid, err := strconv.ParseInt(fmt.Sprint(proj["id"]), 10, 64)
	if err != nil {
		return 0, "", bad("project.json 的项目 ID 不合法")
	}
	taken := func(n string) bool {
		var exists bool
		_ = tx.QueryRow(ctx, `select exists(select 1 from project where workspace_id = $1 and name = $2 and status = 'active')`,
			owner.WorkspaceID, n).Scan(&exists)
		return exists
	}
	name := ImportName(fmt.Sprint(pf.Project["name"]), taken)
	canvas := json.RawMessage(`{}`)
	if len(pf.CanvasData) > 0 && string(pf.CanvasData) != "null" {
		if canvas, err = RemapJSON(pf.CanvasData, ids); err != nil {
			return 0, "", bad("画布数据不是合法 JSON：%v", err)
		}
	}
	proj["name"] = name
	proj["canvas_data"] = canvas
	proj["version"] = 1
	proj["status"] = "active"
	proj["deleted_at"] = nil
	proj["folder_id"] = nil
	proj["create_idempotency_key"] = nil
	now := s.now().UTC()
	proj["created_at"], proj["updated_at"] = now, now
	if err := insert("project", proj); err != nil {
		return 0, "", err
	}

	for _, t := range tables {
		for _, r := range data[t.name] {
			row := Remap(r, ids).(map[string]any)
			own(row, owner)
			fixup(t.name, row, ops, models)
			if err := insert(t.name, row); err != nil {
				return 0, "", err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", err
	}
	return pid, name, nil
}

// own 把归属列换成本机主体。会话、文件夹这类「属于导出机器」的引用置空。
func own(row map[string]any, o Owner) {
	for k := range row {
		switch {
		case k == "workspace_id":
			row[k] = o.WorkspaceID
		case k == "payer_account_id":
			row[k] = o.PayerID
		case k == "user_id" || strings.HasSuffix(k, "_user_id"):
			if row[k] != nil {
				row[k] = o.UserID
			}
		case k == "agent_session_id":
			row[k] = nil
		}
	}
}

// fixup 处理各表的特殊列。
func fixup(t string, row map[string]any, ops, models map[string]string) {
	switch t {
	case "generation":
		// 模型与操作换成本机的 ID（按 code）；本机没配这个模型时给 0：节点照常显示，再生成时提示缺模型（TT-UX-4）。
		if v, ok := ops[fmt.Sprint(row["operation_id"])]; ok {
			row["operation_id"] = json.Number(v)
		}
		if v, ok := models[fmt.Sprint(row["model_id"])]; ok {
			row["model_id"] = json.Number(v)
		} else {
			row["model_id"] = 0
		}
		row["idempotency_key"] = "import:" + fmt.Sprint(row["id"])
		row["retry_of_id"] = nil
		// 进行中的任务在新机器上没有任何执行者：判失败，免得节点永远转圈。
		if st := fmt.Sprint(row["status"]); st == "queued" || st == "running" {
			row["status"] = "failed"
			row["error_code"] = "import_incomplete"
			row["error_message"] = "导出时任务尚未完成"
		}
		row["provider_raw_result"], row["error_detail"], row["provider_task_id"] = nil, nil, nil
		row["heartbeat_at"], row["deadline_at"] = nil, nil
	case "group_item":
		if st := fmt.Sprint(row["status"]); st == "queued" || st == "running" {
			row["status"] = "failed"
			row["error_message"] = "导出时任务尚未完成"
		}
	}
}

// localCatalog 把包里的 旧ID→code 换成 旧ID→本机ID。
func (s *Service) localCatalog(ctx context.Context, c CatalogCodes) (ops, models map[string]string, warns []string, err error) {
	byCode := func(table string) (map[string]string, error) {
		rows, err := s.pool.Query(ctx, `select code, id from `+table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var code string
			var id int64
			if err := rows.Scan(&code, &id); err != nil {
				return nil, err
			}
			out[code] = strconv.FormatInt(id, 10)
		}
		return out, rows.Err()
	}
	localOps, err := byCode("operation")
	if err != nil {
		return nil, nil, nil, err
	}
	localModels, err := byCode("model")
	if err != nil {
		return nil, nil, nil, err
	}
	ops, models = map[string]string{}, map[string]string{}
	for old, code := range c.Operations {
		if id, ok := localOps[code]; ok {
			ops[old] = id
		}
	}
	missing := map[string]bool{}
	for old, code := range c.Models {
		if id, ok := localModels[code]; ok {
			models[old] = id
		} else if !missing[code] {
			missing[code] = true
			warns = append(warns, fmt.Sprintf("本机没有配置模型 %s：已有结果照常显示，重新生成前需在设置里配置或换一个模型", code))
		}
	}
	return ops, models, warns, nil
}

func (s *Service) putEntry(ctx context.Context, f *zip.File, key, contentType string) error {
	rc, err := f.Open()
	if err != nil {
		return bad("读取 %s 失败：%v", f.Name, err)
	}
	defer rc.Close()
	// 声明的大小可以伪造：读的时候再卡一次上限，超出即判包损坏。
	lr := &io.LimitedReader{R: rc, N: int64(f.UncompressedSize64) + 1}
	err = s.store.Put(ctx, key, lr, int64(f.UncompressedSize64), contentType)
	if err == nil && lr.N == 0 {
		err = bad("%s 的实际大小与声明不符", f.Name)
	}
	if err != nil {
		_ = s.store.Delete(context.WithoutCancel(ctx), key) // 写了一半的也删掉
		if IsBadPackage(err) {
			return err
		}
		return fmt.Errorf("写入媒体失败：%w", err)
	}
	return nil
}

// indexEntries 校验全部条目（名字、数量、大小、大小写重名），返回 名字→条目。
func indexEntries(list []*zip.File) (map[string]*zip.File, error) {
	if len(list) > MaxEntries {
		return nil, bad("项目包条目过多（%d）", len(list))
	}
	out := map[string]*zip.File{}
	lower := map[string]bool{}
	var total uint64
	for _, f := range list {
		if strings.HasSuffix(f.Name, "/") {
			continue // 目录条目
		}
		if err := CheckEntryName(f.Name); err != nil {
			return nil, &BadPackageError{Reason: err.Error()}
		}
		l := strings.ToLower(f.Name)
		if lower[l] {
			// 不区分大小写的文件系统上后一个会覆盖前一个。
			return nil, bad("项目包里有重名条目：%q", f.Name)
		}
		lower[l] = true
		if f.UncompressedSize64 > uint64(MaxEntryBytes) {
			return nil, bad("%s 超过单个文件上限", f.Name)
		}
		total += f.UncompressedSize64
		if total > uint64(MaxTotalBytes) {
			return nil, bad("项目包解压后超过总大小上限")
		}
		out[f.Name] = f
	}
	if _, ok := out["manifest.json"]; !ok {
		return nil, bad("项目包缺少 manifest.json")
	}
	return out, nil
}

func readJSON(files map[string]*zip.File, name string, v any) error {
	f, ok := files[name]
	if !ok {
		return bad("项目包缺少 %s", name)
	}
	if f.UncompressedSize64 > uint64(maxJSONBytes) {
		return bad("%s 过大", name)
	}
	rc, err := f.Open()
	if err != nil {
		return bad("读取 %s 失败：%v", name, err)
	}
	defer rc.Close()
	dec := json.NewDecoder(io.LimitReader(rc, maxJSONBytes))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return bad("%s 不是合法 JSON：%v", name, err)
	}
	return nil
}

// IsBadPackage 报告错误是否是「包本身有问题」。
func IsBadPackage(err error) bool {
	var b *BadPackageError
	return errors.As(err, &b)
}
