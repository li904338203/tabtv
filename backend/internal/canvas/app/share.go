package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// ShareProvider 让项目可以被共享（实现 collaboration/app.ResourceProvider）。
//
// ⚠️ 项目**只读共享、不支持导入**（见 collabdomain.Importable 的说明）：
// canvasData 里存着一串产出 ID 与素材 ID，整份搬到另一个工作区后那些引用
// 全部指向别人的资源 —— 画布能打开，但每个节点的产出都是裂的，而这不报错。
type ShareProvider struct{ svc *Service }

func NewShareProvider(s *Service) *ShareProvider { return &ShareProvider{svc: s} }

func (p *ShareProvider) ResourceType() collabdomain.ResourceType {
	return collabdomain.ResourceProject
}

// Describe 取一份共享视图。
//
// detail=false 时**不取 canvasData**：一张"共享给我的"列表可能有几十行，
// 每份画布 JSON 几百 KB 到几 MB，全取回来只为显示名字，是把内存白白烧掉
// （与列表查询不取 canvas_data 是同一条）。
func (p *ShareProvider) Describe(ctx context.Context, id int64, detail bool) (collab.ResourceView, error) {
	var pr repo.Project
	var err error
	if detail {
		pr, err = p.svc.repo.GetForShare(ctx, id)
	} else {
		pr, err = p.svc.repo.MetaForShare(ctx, id)
	}
	if errors.Is(err, repo.ErrNotFound) {
		return collab.ResourceView{}, collab.ErrResourceGone
	}
	if err != nil {
		return collab.ResourceView{}, err
	}
	v := collab.ResourceView{
		ID: pr.ID, WorkspaceID: pr.WorkspaceID, Name: pr.Name,
		Kind: "project", UpdatedAt: pr.UpdatedAt,
	}
	if detail {
		// ⚠️ 明确列字段，不把仓储结构体整个序列化出去：加一个内部列就会漏给
		// 一个本来不该看到它的人，而这不报错（同脚本导出那条规则）。
		// 尤其是 version —— 受让方拿到它会以为自己能拿它去保存，
		// 而写路径在 P12-01 里还没有对共享开放（见出口挂账）。
		v.Detail, err = json.Marshal(struct {
			CanvasData json.RawMessage `json:"canvasData"`
			CoverMedia string          `json:"coverMediaId,omitempty"`
		}{CanvasData: pr.CanvasData, CoverMedia: idOrEmpty(pr.CoverMedia)})
		if err != nil {
			return collab.ResourceView{}, err
		}
	}
	return v, nil
}

func idOrEmpty(v int64) string {
	if v == 0 {
		return ""
	}
	return itoa(v)
}
