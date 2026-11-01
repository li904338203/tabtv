package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 组节点与子项（AV-21~AV-26）。
//
// ⚠️ 子项**拆表存、不塞 JSONB**（07 §6）：逐项重试、逐项计费、部分成功
// 都要求对单个子项做**带条件的更新**。整块 JSONB 读改写在两个子项同时
// 回写时必丢更新 —— 而那正是批量执行的常态。

var (
	ErrGroupNotFound     = errors.New("组节点不存在")
	ErrGroupDuplicate    = errors.New("该画布节点已经有组了")
	ErrGroupConflict     = errors.New("组已被他人修改")
	ErrGroupItemNotFound = errors.New("组子项不存在")
)

// GroupNode 是一个组节点。
type GroupNode struct {
	ID           int64
	WorkspaceID  int64
	ProjectID    int64
	CanvasNodeID string
	GroupType    string
	ScriptID     int64
	Name         string
	Status       string
	Version      int
}

const groupCols = `select id, workspace_id, project_id, canvas_node_id, group_type,
	coalesce(script_id,0), coalesce(name,''), status, version from group_node `

func scanGroup(row pgx.Row) (GroupNode, error) {
	var g GroupNode
	err := row.Scan(&g.ID, &g.WorkspaceID, &g.ProjectID, &g.CanvasNodeID, &g.GroupType,
		&g.ScriptID, &g.Name, &g.Status, &g.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupNode{}, ErrGroupNotFound
	}
	return g, err
}

// GroupItem 是组里的一项。
type GroupItem struct {
	ID            int64
	GroupNodeID   int64
	ItemNo        int
	ShotID        int64
	ScriptAssetID int64
	GenerationID  int64
	// ShotVersion 是建组时镜头的版本。镜头之后改过就说明这一项过期了。
	ShotVersion    int
	ParamsSnapshot json.RawMessage
	InputRefs      json.RawMessage
	Status         string
	Progress       int
	CreditsCost    int
	ErrorMessage   string
}

const itemCols = `select id, group_node_id, item_no, coalesce(shot_id,0),
	coalesce(script_asset_id,0), coalesce(generation_id,0), coalesce(shot_version,0),
	params_snapshot, input_refs, status, progress, credits_cost,
	coalesce(error_message,'') from group_item `

func scanItem(row pgx.Row) (GroupItem, error) {
	var it GroupItem
	err := row.Scan(&it.ID, &it.GroupNodeID, &it.ItemNo, &it.ShotID, &it.ScriptAssetID,
		&it.GenerationID, &it.ShotVersion, &it.ParamsSnapshot, &it.InputRefs,
		&it.Status, &it.Progress, &it.CreditsCost, &it.ErrorMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return GroupItem{}, ErrGroupItemNotFound
	}
	return it, err
}

// ---------------------------------------------------------------- 组

// InsertGroup 建组，并在同一个事务里插入全部子项。
//
// ⚠️ 必须一个事务：分两次写的话，子项插一半失败会留下一个**空组** ——
// 画布上多出一个点不动也删不掉的节点，而用户只知道"建组失败了"。
func (r *Repo) InsertGroup(ctx context.Context, g GroupNode, items []GroupItem) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		insert into group_node (id, workspace_id, project_id, canvas_node_id,
			group_type, script_id, name, status)
		values ($1,$2,$3,$4,$5,nullif($6,0::bigint),nullif($7,''),$8)`,
		g.ID, g.WorkspaceID, g.ProjectID, g.CanvasNodeID, g.GroupType,
		g.ScriptID, g.Name, g.Status)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrGroupDuplicate
		}
		return err
	}
	for _, it := range items {
		if err := insertItemTx(ctx, tx, it); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func insertItemTx(ctx context.Context, tx pgx.Tx, it GroupItem) error {
	_, err := tx.Exec(ctx, `
		insert into group_item (id, group_node_id, item_no, shot_id, script_asset_id,
			shot_version, params_snapshot, input_refs, status, credits_cost)
		values ($1,$2,$3,nullif($4,0::bigint),nullif($5,0::bigint),nullif($6,0),
			coalesce($7,'{}'::jsonb), coalesce($8,'[]'::jsonb), $9, $10)`,
		it.ID, it.GroupNodeID, it.ItemNo, it.ShotID, it.ScriptAssetID, it.ShotVersion,
		it.ParamsSnapshot, it.InputRefs, it.Status, it.CreditsCost)
	return err
}

// GetGroup 按 ID 取组（带工作区归属校验）。
func (r *Repo) GetGroup(ctx context.Context, workspaceID, id int64) (GroupNode, error) {
	return scanGroup(r.pool.QueryRow(ctx,
		groupCols+`where id=$1 and workspace_id=$2`, id, workspaceID))
}

// GetGroupByNode 按画布节点取组 —— 用户重新打开节点时走这里（同 AV-05）。
func (r *Repo) GetGroupByNode(ctx context.Context, workspaceID, projectID int64,
	nodeID string) (GroupNode, error) {
	return scanGroup(r.pool.QueryRow(ctx,
		groupCols+`where workspace_id=$1 and project_id=$2 and canvas_node_id=$3`,
		workspaceID, projectID, nodeID))
}

// ListGroups 列出一个脚本下的全部组。
func (r *Repo) ListGroups(ctx context.Context, workspaceID, scriptID int64) ([]GroupNode, error) {
	rows, err := r.pool.Query(ctx,
		groupCols+`where workspace_id=$1 and script_id=$2 order by created_at`,
		workspaceID, scriptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupNode
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UpdateGroupStatus 按**当前版本**更新组状态并进位版本（FLOW §5.2）。
//
// ⚠️ execute / retry / ungroup 都是写操作，都要带版本 —— 不能只在改名字
// 的时候做乐观锁。两个人同时点"执行"，没有乐观锁就是两批任务、两份钱。
func (r *Repo) UpdateGroupStatus(ctx context.Context, id int64, status string,
	expectedVersion int) (GroupNode, error) {
	tag, err := r.pool.Exec(ctx, `
		update group_node set status=$2, version=version+1, updated_at=now()
		where id=$1 and ($3 = 0 or version = $3)`, id, status, expectedVersion)
	if err != nil {
		return GroupNode{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := scanGroup(r.pool.QueryRow(ctx, groupCols+`where id=$1`, id)); err != nil {
			return GroupNode{}, err
		}
		return GroupNode{}, ErrGroupConflict
	}
	return scanGroup(r.pool.QueryRow(ctx, groupCols+`where id=$1`, id))
}

// RefreshGroupStatus 按子项重算聚合状态（不动版本）。
//
// ⚠️ **不进位版本**：这是服务端根据子项回写自己算出来的，不是用户的编辑。
// 进位的话用户手上那份 version 会被后台的异步回写不断顶掉，
// 他点什么都是 409 —— 而他什么都没改。
func (r *Repo) RefreshGroupStatus(ctx context.Context, id int64, status string) error {
	_, err := r.pool.Exec(ctx,
		`update group_node set status=$2, updated_at=now() where id=$1`, id, status)
	return err
}

// ---------------------------------------------------------------- 子项

// ListItems 列出组内子项（按 itemNo）。
func (r *Repo) ListItems(ctx context.Context, groupID int64) ([]GroupItem, error) {
	rows, err := r.pool.Query(ctx,
		itemCols+`where group_node_id=$1 order by item_no`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetItem 取单个子项（带组归属校验）。
func (r *Repo) GetItem(ctx context.Context, groupID, itemID int64) (GroupItem, error) {
	return scanItem(r.pool.QueryRow(ctx,
		itemCols+`where id=$1 and group_node_id=$2`, itemID, groupID))
}

// SaveItemInput 是子项的可编辑字段（draft 期间）。
type SaveItemInput struct {
	GroupID        int64
	ItemID         int64
	ParamsSnapshot json.RawMessage
	InputRefs      json.RawMessage
	// ShotVersion 非 0 时把快照对齐到镜头的当前版本。
	//
	// ⚠️ 这是 `stale` 的**唯一出路**。不刷新版本号的话，子项状态虽然
	// 回到 draft，提交时仍然会被 shotVersion 比对拦下 ——
	// 用户改了一次镜头，这一项就再也跑不了了，界面上还看不出为什么。
	ShotVersion int
}

// SaveItem 编辑 draft 子项的参数与输入。
//
// ⚠️ 只允许改 `draft` / `failed` / `stale` 的项：已经在跑或已经成功的项
// 改了参数，库里存的就不再是那次任务真正用过的输入 ——
// 而排查"为什么出来的图不对"时，快照是唯一的证据。
func (r *Repo) SaveItem(ctx context.Context, in SaveItemInput) (GroupItem, error) {
	tag, err := r.pool.Exec(ctx, `
		update group_item set
			params_snapshot = coalesce($3, params_snapshot),
			input_refs      = coalesce($4, input_refs),
			shot_version    = case when $5::int > 0 then $5::int else shot_version end,
			-- ⚠️ 刷新后一律回到 draft，不只是 stale 那一种。
			-- 这一项的输入已经按当前上游重算过了，上一次失败的原因
			-- 不再描述它；留着 'failed' 而把 error_message 清空，
			-- 界面上就是一个红叉配一句空白说明。
			status          = 'draft',
			error_message   = null,
			updated_at = now()
		where id=$1 and group_node_id=$2 and status in ('draft','failed','stale')`,
		in.ItemID, in.GroupID, in.ParamsSnapshot, in.InputRefs, in.ShotVersion)
	if err != nil {
		return GroupItem{}, err
	}
	if tag.RowsAffected() == 0 {
		it, err := r.GetItem(ctx, in.GroupID, in.ItemID)
		if err != nil {
			return GroupItem{}, err
		}
		return GroupItem{}, errors.New("子项当前状态不允许编辑：" + it.Status)
	}
	return r.GetItem(ctx, in.GroupID, in.ItemID)
}

// MarkItemQueued 把子项标为已提交并挂上 generation。
//
// ⚠️ 带状态条件（CAS）：并发点两次"执行"时，第二次的 0 行更新就是
// "别人已经提交过了"，而不是再交一次 —— 再交一次就是再扣一次钱。
func (r *Repo) MarkItemQueued(ctx context.Context, itemID, generationID int64,
	credits int) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update group_item set generation_id=$2, credits_cost=$3, status='queued',
			progress=0, error_message=null, updated_at=now()
		where id=$1 and status in ('draft','failed','cancelled','stale')`,
		itemID, generationID, credits)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkItemFailed 把子项标为失败（提交阶段就失败时用，没有 generation）。
func (r *Repo) MarkItemFailed(ctx context.Context, itemID int64, msg string) error {
	_, err := r.pool.Exec(ctx, `
		update group_item set status='failed', error_message=left($2,1024), updated_at=now()
		where id=$1 and status not in ('running','queued')`, itemID, msg)
	return err
}

// ItemByGeneration 按 generation 反查子项 —— Worker 回写时用。
func (r *Repo) ItemByGeneration(ctx context.Context, generationID int64) (GroupItem, error) {
	return scanItem(r.pool.QueryRow(ctx,
		itemCols+`where generation_id=$1`, generationID))
}

// BindItemOutcome 按 generation 回写子项终态。
//
// ⚠️ 只在子项**还没到终态**时生效：晚到的结果不能把已经取消的项改成成功，
// 也不能把已经重试过的旧任务结果盖到新的上面。
//
// ⚠️ 回写失败**只告警不回滚生成**：任务已经成功、产物已经归档，
// 为一次回写把它判失败会让用户白花一次钱。代价是子项可能停在 running，
// 恢复扫描会兜住。
func (r *Repo) BindItemOutcome(ctx context.Context, generationID int64,
	status, errMsg string) (int64, error) {
	var groupID int64
	// ⚠️ `$2` 要显式转型：同一个占位符既当 status 写入又参与 `case when`
	// 比较时，PG 推不出一致的类型，直接报 42P08 —— 而这只有真库跑得出来，
	// 编译和 mock 都看不见。
	err := r.pool.QueryRow(ctx, `
		update group_item set status=$2::varchar,
			progress = case when $2::varchar = 'success' then 100 else progress end,
			error_message = nullif(left($3,1024),''), updated_at=now()
		where generation_id=$1 and status not in ('success','failed','cancelled')
		returning group_node_id`, generationID, status, errMsg).Scan(&groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // 已是终态或不是组任务：什么都不做。
	}
	return groupID, err
}

// MarkItemsStale 把引用了某个镜头的 draft 子项标为过期（FLOW-05）。
//
// ⚠️ 只动还没跑的：已经跑完的项，它那次用的就是当时的快照，
// 把它标成 stale 等于说"这张图不对"，而它当时是对的。
func (r *Repo) MarkItemsStale(ctx context.Context, shotID int64, shotVersion int) error {
	_, err := r.pool.Exec(ctx, `
		update group_item set status='stale', updated_at=now()
		where shot_id=$1 and status='draft'
		  and coalesce(shot_version,0) <> 0 and coalesce(shot_version,0) < $2`,
		shotID, shotVersion)
	return err
}

// CancellableItems 列出组内还能取消的子项（对应的 generation）。
func (r *Repo) CancellableItems(ctx context.Context, groupID int64) ([]GroupItem, error) {
	rows, err := r.pool.Query(ctx,
		itemCols+`where group_node_id=$1 and status in ('queued','running') order by item_no`,
		groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkItemCancelled 把子项标成已取消。
func (r *Repo) MarkItemCancelled(ctx context.Context, itemID int64) error {
	_, err := r.pool.Exec(ctx, `
		update group_item set status='cancelled', updated_at=now()
		where id=$1 and status in ('queued','running','draft','stale')`, itemID)
	return err
}

// ---------------------------------------------------------------- 输入素材

// PlanImage 与 domain.PlanImage 同形，放在这里避免 repo 反向依赖 domain。
type PlanImage struct {
	GenerationID int64
	MediaID      int64
	Kind         string
}

// LatestShotImages 取某镜**最新一次成功**的分镜图（AV-21）。
//
// ⚠️ 只取最新一次：一镜可能出过好几版图，把历史版本都当参考发上去，
// 模型会看到同一个画面的多个版本，出来的东西两边都不像。
//
// ⚠️ 只认 success：跑失败的那次没有产物，取到了也只是一个空引用。
func (r *Repo) LatestShotImages(ctx context.Context, shotID int64) ([]PlanImage, error) {
	rows, err := r.pool.Query(ctx, `
		select g.id
		from generation g
		where g.shot_id = $1 and g.status = 'success' and g.result_type = 'image'
		order by g.finished_at desc nulls last, g.id desc
		limit 1`, shotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanImage
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, PlanImage{GenerationID: id, Kind: "storyboard"})
	}
	return out, rows.Err()
}

// ShotAssetsWithoutImage 列出某镜引用了、却还没有可用设定图的资产名（按引用顺序，CD-82）。
//
// ⚠️ ShotAssetMedia 会**跳过**它们，而提示词里「参考图对应关系：图N = 名称」是按**全部**引用编号的 ——
// 少一张图，它后面每一张的编号都错一位：模型把 A 的特征画到 B 身上，而任务是成功的。
func (r *Repo) ShotAssetsWithoutImage(ctx context.Context, shotID int64) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		select a.name
		from shot_asset_ref ref
		join script_asset a on a.id = ref.script_asset_id
		where ref.shot_id = $1 and not (a.status = 'ready' and a.media_object_id is not null)
		order by ref.sort`, shotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ShotAssetMedia 取某镜引用的资产图，**按引用顺序**（sort 升序）。
//
// ⚠️ 顺序即参考图的锚定顺序：错了模型会把 A 的特征画到 B 身上，
// 而画面看上去挺正常 —— 这种错最难发现。
//
// ⚠️ 只取已就绪、且真有媒体的：pending/failed 的资产没有图，
// 带上去就是一个解析不出来的引用，整条任务受理失败。
func (r *Repo) ShotAssetMedia(ctx context.Context, shotID int64) ([]PlanImage, error) {
	rows, err := r.pool.Query(ctx, `
		select a.media_object_id
		from shot_asset_ref ref
		join script_asset a on a.id = ref.script_asset_id
		where ref.shot_id = $1 and a.status = 'ready' and a.media_object_id is not null
		order by ref.sort`, shotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanImage
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, PlanImage{MediaID: id, Kind: "asset"})
	}
	return out, rows.Err()
}

// ShotVideo 是一镜的成片素材（供时间线按分镜装配）。
type ShotVideo struct {
	ShotID  int64
	ShotNo  int
	MediaID int64
	// Status 是那一项的生成状态；MediaID 为 0 时靠它说明原因。
	Status       string
	ErrorMessage string
}

// ShotVideos 按镜号顺序给出每一镜的视频产出。
//
// ⚠️ **一镜可能有好几条视频组子项**：用户可以为同一个脚本建两个视频组，
// 单项重试也会留下旧的那条。不挑一条的话同一镜会在装配计划里出现两次 ——
// 联调里表现为"4 镜的脚本报出 6 个缺口"，而那看起来完全不像是重复。
// 挑法是**成功的优先、然后取最新的**：重试之后旧的那条仍然留在库里。
//
// ⚠️ **左连接**而不是内连接：内连接会把"还没生成"的镜头整行丢掉，
// 于是时间线那边看到的是一个"镜头更少但完整"的脚本 —— 而 NEW-09
// 要求的恰恰是把缺口报出来。左连接让那些镜头带着空的 media 出现。
//
// ⚠️ 只认**视频组**（group_type='video_generator'）的产出：分镜图组出的是图，
// 装进画面轨会变成一段静止画面 —— 那可能是用户想要的，但不该是"自动装配"
// 替他决定的。
func (r *Repo) ShotVideos(ctx context.Context, workspaceID, scriptID int64) (
	[]ShotVideo, error) {
	rows, err := r.pool.Query(ctx, `
		select s.id, s.shot_no,
		       coalesce(best.media_object_id, 0),
		       coalesce(best.status, ''),
		       coalesce(best.error_message, '')
		from shot s
		join script_instance si on si.id = s.script_id and si.workspace_id = $1
		left join lateral (
		    select gi.status, gi.error_message,
		           (select o.media_object_id from generation_output o
		            where o.generation_id = gi.generation_id
		            order by o.idx limit 1) as media_object_id
		    from group_item gi
		    join group_node gn on gn.id = gi.group_node_id
		    where gi.shot_id = s.id
		      and gn.script_id = s.script_id
		      and gn.group_type = 'video_generator'
		    order by (gi.status = 'success') desc, gi.id desc
		    limit 1
		) best on true
		where s.script_id = $2
		order by s.shot_no`, workspaceID, scriptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShotVideo
	for rows.Next() {
		var v ShotVideo
		if err := rows.Scan(&v.ShotID, &v.ShotNo, &v.MediaID, &v.Status,
			&v.ErrorMessage); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// BoardShot 是分镜成片要的一镜：台词、画面描述、计划时长，以及这一镜挑出来的视频产出（docs/agent/smartcut SCT-28）。
type BoardShot struct {
	ShotID       int64
	ShotNo       int
	DurationSec  int
	Description  string
	Dialogue     string
	MediaID      int64
	Status       string
	ErrorMessage string
}

// BoardShots 按镜号给出每一镜的视频产出与台词。挑法与 ShotVideos 相同（成功的优先、再取最新的，左连接保留缺失的镜头）。
//
// ⚠️ groupID > 0 时**只取这个视频组的子项**：同一脚本可能有好几个视频组（CLAUDE.md §5.10「一镜可能有好几条视频组子项」），
// 画布上连的是哪一个就用哪一个，不按镜号去猜。
func (r *Repo) BoardShots(ctx context.Context, workspaceID, scriptID, groupID int64) ([]BoardShot, error) {
	rows, err := r.pool.Query(ctx, `
		select s.id, s.shot_no, s.duration_sec, coalesce(s.description, ''), coalesce(s.dialogue, ''),
		       coalesce(best.media_object_id, 0), coalesce(best.status, ''), coalesce(best.error_message, '')
		from shot s
		join script_instance si on si.id = s.script_id and si.workspace_id = $1
		left join lateral (
		    select gi.status, gi.error_message,
		           (select o.media_object_id from generation_output o
		            where o.generation_id = gi.generation_id
		            order by o.idx limit 1) as media_object_id
		    from group_item gi
		    join group_node gn on gn.id = gi.group_node_id
		    where gi.shot_id = s.id
		      and gn.script_id = s.script_id
		      and gn.group_type = 'video_generator'
		      and ($3::bigint = 0 or gn.id = $3)
		    order by (gi.status = 'success') desc, gi.id desc
		    limit 1
		) best on true
		where s.script_id = $2
		order by s.shot_no`, workspaceID, scriptID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoardShot
	for rows.Next() {
		var v BoardShot
		if err := rows.Scan(&v.ShotID, &v.ShotNo, &v.DurationSec, &v.Description, &v.Dialogue, &v.MediaID, &v.Status,
			&v.ErrorMessage); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
