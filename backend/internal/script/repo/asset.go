package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Asset 是资产行。
type Asset struct {
	ID            int64
	WorkspaceID   int64
	Type          string
	Name          string
	Description   string
	Profile       json.RawMessage
	MediaObjectID int64
	Source        string
	Status        string
	// UpdatedAt 只在共享视图里填（列表查询不取，省一列扫描）。
	UpdatedAt time.Time
}

const assetCols = `select id, workspace_id, asset_type, name, coalesce(description,''),
	coalesce(profile,'{}'::jsonb), coalesce(media_object_id,0), source, status
	from script_asset `

func scanAsset(row pgx.Row) (Asset, error) {
	var a Asset
	err := row.Scan(&a.ID, &a.WorkspaceID, &a.Type, &a.Name, &a.Description,
		&a.Profile, &a.MediaObjectID, &a.Source, &a.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	return a, err
}

// ListAssetsByScript 列出**本脚本**的资产。
//
// ⚠️ `script_asset` 表上**没有 script_id**（它是工作区级的资产库），
// 归属靠 `shot_asset_ref → shot.script_id` 反查。这正是
// 「自动识别只在本脚本内归并」的实现方式 —— 老系统按用户级同名匹配，
// 结果新节点里的 `@唐僧` 命中数周前的记录，把别的节点的成品图
// 和 failed 状态一起继承过来。
func (r *Repo) ListAssetsByScript(ctx context.Context, scriptID int64) ([]Asset, error) {
	rows, err := r.pool.Query(ctx, assetCols+`
		where id in (select distinct ar.script_asset_id
		               from shot_asset_ref ar join shot s on s.id = ar.shot_id
		              where s.script_id = $1)
		order by asset_type, name`, scriptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAssetsByWorkspace 工作区级资产库（AV-20 的跨脚本复用入口）。
//
// ⚠️ 它只用于**用户手动挑选**，绝不参与自动识别的同名归并。
func (r *Repo) ListAssetsByWorkspace(ctx context.Context, workspaceID int64,
	assetType, keyword string, limit int) ([]Asset, error) {
	rows, err := r.pool.Query(ctx, assetCols+`
		where workspace_id = $1
		  and ($2 = '' or asset_type = $2)
		  and ($3 = '' or name ilike '%' || $3 || '%')
		order by updated_at desc limit $4`,
		workspaceID, assetType, keyword, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAssetsByScriptOrWorkspace 取"本脚本已引用的 + 本工作区新建但还没引用的"资产。
//
// ⚠️ reconcile 刚插完资产时，它们还没有任何 shot_asset_ref —— 只按引用反查
// 会一个都查不到，于是引用永远绑不上。这里按**名字**把工作区里还没被引用的
// 新资产也带进来，专供 reconcile 之后的重绑定使用。
//
// ⚠️ 它**不能**用在自动归并上：归并的范围必须是本脚本，
// 否则新节点里的 `@唐僧` 会命中别的节点的记录（老系统的真实事故）。
func (r *Repo) ListAssetsByScriptOrWorkspace(ctx context.Context,
	workspaceID, scriptID int64) ([]Asset, error) {
	rows, err := r.pool.Query(ctx, assetCols+`
		where workspace_id = $1
		  and (id in (select distinct ar.script_asset_id
		                from shot_asset_ref ar join shot s on s.id = ar.shot_id
		               where s.script_id = $2)
		       or id not in (select script_asset_id from shot_asset_ref))
		order by asset_type, name`, workspaceID, scriptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repo) GetAsset(ctx context.Context, workspaceID, id int64) (Asset, error) {
	return scanAsset(r.pool.QueryRow(ctx, assetCols+
		`where id = $1 and workspace_id = $2`, id, workspaceID))
}

func (r *Repo) InsertAsset(ctx context.Context, a Asset) error {
	profile := a.Profile
	if len(profile) == 0 {
		profile = json.RawMessage(`{}`)
	}
	_, err := r.pool.Exec(ctx, `
		insert into script_asset (id, workspace_id, asset_type, name, description,
			profile, media_object_id, source, status)
		values ($1,$2,$3,$4,nullif($5,''),$6,nullif($7::bigint,0),$8,$9)`,
		a.ID, a.WorkspaceID, a.Type, a.Name, a.Description, profile,
		a.MediaObjectID, a.Source, a.Status)
	return err
}

// UpdateAssetInput 是一次资产更新。
type UpdateAssetInput struct {
	ID            int64
	WorkspaceID   int64
	Type          *string
	Name          *string
	Description   *string
	Profile       json.RawMessage
	MediaObjectID *int64
	Source        *string
	Status        *string
}

// UpdateAsset 部分更新。
//
// ⚠️ 资产没有版本号（表上没有 version 列）：它不是多人同时编辑的对象，
// 而且每个字段都是"最后写入的赢"这种语义。真正需要乐观锁的是脚本与镜头
// —— 那两个会被并发编辑，也有明确的"整体一致"要求。
func (r *Repo) UpdateAsset(ctx context.Context, in UpdateAssetInput) (Asset, error) {
	tag, err := r.pool.Exec(ctx, `
		update script_asset set
			asset_type      = coalesce($3, asset_type),
			name            = coalesce($4, name),
			description     = coalesce($5, description),
			profile         = coalesce($6, profile),
			media_object_id = coalesce($7, media_object_id),
			source          = coalesce($8, source),
			status          = coalesce($9, status),
			updated_at = now()
		where id = $1 and workspace_id = $2`,
		in.ID, in.WorkspaceID, in.Type, in.Name, in.Description, nilJSON(in.Profile),
		in.MediaObjectID, in.Source, in.Status)
	if err != nil {
		return Asset{}, err
	}
	if tag.RowsAffected() == 0 {
		return Asset{}, ErrNotFound
	}
	return r.GetAsset(ctx, in.WorkspaceID, in.ID)
}

// ---------------------------------------------------------------- 镜头引用

// ReplaceShotRefs 重写一个镜头的资产引用。
//
// ⚠️ 整体替换而不是增量：引用的 `sort` 决定参考图的锚定顺序，
// 与提示词里提到它们的顺序一一对应。增量改的话，中间插一个就得
// 把后面全部重排 —— 那本来就是整体替换。
func (r *Repo) ReplaceShotRefs(ctx context.Context, shotID int64,
	nextID func() int64, refs []ShotRef) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`delete from shot_asset_ref where shot_id = $1`, shotID); err != nil {
		return err
	}
	for _, ref := range refs {
		if _, err := tx.Exec(ctx, `
			insert into shot_asset_ref (id, shot_id, script_asset_id, sort)
			values ($1,$2,$3,$4)`, nextID(), shotID, ref.AssetID, ref.Sort); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ShotRef 是一条镜头 → 资产的引用行。
type ShotRef struct {
	AssetID int64
	Sort    int
}

// ListShotRefs 取一个脚本下全部镜头的资产引用（按 sort）。
func (r *Repo) ListShotRefs(ctx context.Context, scriptID int64) (map[int64][]ShotRef, error) {
	rows, err := r.pool.Query(ctx, `
		select ar.shot_id, ar.script_asset_id, ar.sort
		from shot_asset_ref ar join shot s on s.id = ar.shot_id
		where s.script_id = $1 order by ar.shot_id, ar.sort`, scriptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]ShotRef{}
	for rows.Next() {
		var shotID int64
		var ref ShotRef
		if err := rows.Scan(&shotID, &ref.AssetID, &ref.Sort); err != nil {
			return nil, err
		}
		out[shotID] = append(out[shotID], ref)
	}
	return out, rows.Err()
}

// AssetUsage 是一个资产被哪些镜头引用。
func (r *Repo) AssetUsage(ctx context.Context, assetID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`select count(*) from shot_asset_ref where script_asset_id = $1`, assetID).Scan(&n)
	return n, err
}

// MarkAssetResult 回写资产图的生成结果。
//
// ⚠️ 只在资产还处于 `generating` 时写：用户在任务跑的途中改成了上传，
// 或者手动把状态改掉了，晚到的结果不该把他的动作覆盖回去
// （与生成域"终态不被晚到结果覆盖"是同一条规则）。
func (r *Repo) MarkAssetResult(ctx context.Context, assetID, mediaID int64,
	status string) error {
	_, err := r.pool.Exec(ctx, `
		update script_asset
		set media_object_id = coalesce(nullif($2::bigint,0), media_object_id),
		    status = $3, updated_at = now()
		where id = $1 and status = 'generating'`, assetID, mediaID, status)
	return err
}

// MarkPromptsStaleByAsset 把引用了某个资产的镜头的提示词标 stale。
func (r *Repo) MarkPromptsStaleByAsset(ctx context.Context, assetID int64) error {
	_, err := r.pool.Exec(ctx, `
		update shot set
			image_prompt_status = case when image_prompt_status = 'ready'
			                           then 'stale' else image_prompt_status end,
			video_prompt_status = case when video_prompt_status = 'ready'
			                           then 'stale' else video_prompt_status end,
			updated_at = now()
		where id in (select shot_id from shot_asset_ref where script_asset_id = $1)`,
		assetID)
	return err
}

// GetAssetForShare 按 ID 取资产，**不带 workspace_id 条件**。
//
// ⚠️ 这是本仓储里唯一绕过租户条件的查询，只允许共享域在校验完
// resource_share 授权之后调用。受让方本来就不在源工作区里，带上
// workspace_id 只会永远查不到 —— 但也正因为它不带，**绝不能**把它
// 接到任何直接由请求参数驱动的接口上。归属由返回的 workspace_id 交给
// 调用方核对。
func (r *Repo) GetAssetForShare(ctx context.Context, id int64) (Asset, error) {
	return scanAssetWithTime(r.pool.QueryRow(ctx, `
		select id, workspace_id, asset_type, name, coalesce(description,''),
			coalesce(profile,'{}'::jsonb), coalesce(media_object_id,0), source, status,
			updated_at
		from script_asset where id = $1`, id))
}

func scanAssetWithTime(row pgx.Row) (Asset, error) {
	var a Asset
	err := row.Scan(&a.ID, &a.WorkspaceID, &a.Type, &a.Name, &a.Description,
		&a.Profile, &a.MediaObjectID, &a.Source, &a.Status, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	return a, err
}
