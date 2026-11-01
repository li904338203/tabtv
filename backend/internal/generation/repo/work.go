package repo

import (
	"context"

	"github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// RegisterWork 把一次成功的媒体生成登记成一件**待审**作品。
//
// 起因：产品决定用户不再手动提交作品 —— 每次成功的生成（图片/视频/音频，
// 不含文本）自动进作品库，管理端「作品审核」直接审。写法照抄成片
// （`timeline/repo.InsertWork`）：拥有来源任务的域自己写 work 行，
// 因为只有它知道「什么算成功、产出在哪一项」，而 Worker 不依赖内容域是有意的。
//
// ⚠️ **进作品库不等于公开发布**：`review_status='pending'`、`submitted_at` 为空、
// `is_featured=false`。公开可见性仍是 `approved ∧ featured ∧ ¬offline`。
//
// ⚠️ 媒体取法与 API 侧的 `workSourceGuard` 相同：**按 idx 第一个有媒体的产出**，
// 不硬取 outputs[0]（一次生成可能先出一段文字再出图）。迁移 00057 的历史补登
// 用的是同一个取法 —— 改这里要同步改那边的口径说明（迁移本身不可改）。
//
// ⚠️ 标题取项目名并在 SQL 里截断（`work.title` 是 varchar(100)），取不到给
// 「未命名作品」（与内容域 `normalizeWorkTitle` 的默认值一致）。
//
// ⚠️ 幂等靠 `uk_work_source(source_type, source_id)` 且 **do nothing**：
// 已登记（含已审过的）一律不碰 —— 重复消息不能把「已驳回」洗回「待审」。
// 返回本次是否真的新登记了一件。
//
// ⚠️ 营销「生成角色」的候选图**不登记**（MKT-T-35、dev/05 MKR-17）：按快照里的来源标识
// `_snapshot.origin` 排除，不按 `project_id = 0` 反推（见 domain.OriginMarketingCharacter）。
// 那是一批要用户挑的草图，进审核队列只会让运营面对一屏「未命名作品」。
//
// ⚠️ 智能剪辑名下的片段级生成同样不登记（docs/agent/smartcut SCK-7）：那是剪辑台里的一段素材，
// 要发布的是导出的成片，由用户显式提交作品。
func (r *Repo) RegisterWork(ctx context.Context, id, generationID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		insert into work (id, workspace_id, source_type, source_id,
			media_object_id, title, review_status)
		select $1, g.workspace_id, 'generation', g.id, o.media_object_id,
		       coalesce(nullif(left(btrim(p.name),100),''), '未命名作品'), 'pending'
		from generation g
		join lateral (
			select x.media_object_id from generation_output x
			where x.generation_id = g.id and x.media_object_id is not null
			  and x.output_type in ('image','video','audio')
			order by x.idx limit 1
		) o on true
		left join project p on p.id = g.project_id
		where g.id = $2 and g.status = 'success' and g.result_type <> 'text'
		  and coalesce(g.params_snapshot->'_snapshot'->>'origin', '') <> all($3)
		on conflict (source_type, source_id) do nothing`, id, generationID, unregisteredOrigins)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// unregisteredOrigins 不登记作品的来源（快照里的 `_snapshot.origin`）。
var unregisteredOrigins = []string{domain.OriginMarketingCharacter, domain.OriginSmartcut}
