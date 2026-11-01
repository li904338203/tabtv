package app

import (
	"context"

	"github.com/li904338203/tabtv/backend/internal/content/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 我的作品（成功产出）

// validCreationType 判断筛选值是否在「创作资产」收录的类型里（清单只有一份：repo.CreationTypes）。
// 空串 = 全部。
func validCreationType(t string) bool {
	if t == "" {
		return true
	}
	for _, v := range repo.CreationTypes {
		if v == t {
			return true
		}
	}
	return false
}

// CreationPage 一页「我的作品」。
type CreationPage struct {
	Items   []repo.Creation
	HasMore bool
	// Total 与列表同一套筛选条件（不含游标）。取不到时为 -1：页头少一个数字而已，
	// 不该让整页作品打不开。
	Total int
}

// Creations 列出工作区里成功的生成（老系统「我的作品」的语义）。
//
// ⚠️ 与 `MyWorks`（提交去审核的作品，P13-02）是两件事，别合并：
// 一个回答「我生成过什么」，一个回答「我拿去投稿的是什么、审到哪了」。
func (s *Service) Creations(ctx context.Context, workspaceID int64, resultType string,
	limit int, cursorID int64, projectID ...int64) (CreationPage, error) {
	var pid int64
	if len(projectID) > 0 {
		pid = projectID[0]
	}
	if !validCreationType(resultType) {
		// 拒绝而不是忽略：静默当成「全部」的话，传了一个错的类型却得到一整页别的东西，
		// 调用方会以为筛选坏了。`text` 也在这里被拒 —— 创作资产不收文本。
		return CreationPage{}, httpx.Errf(errcode.InvalidArgument, "作品类型非法（只支持 image / video / audio）")
	}
	if limit <= 0 || limit > 100 {
		limit = 24
	}
	f := repo.CreationFilter{
		WorkspaceID: workspaceID, ResultType: resultType, ProjectID: pid, Limit: limit + 1, CursorID: cursorID,
	}
	rows, err := s.repo.ListCreations(ctx, f)
	if err != nil {
		return CreationPage{}, httpx.Wrap(errcode.Internal, err)
	}
	page := CreationPage{Items: rows}
	// 多取一条来判断还有没有下一页 —— 比「这页正好满了」更可靠：
	// 最后一页正好满页时后者会让前端再空跑一次请求。
	if len(rows) > limit {
		page.Items, page.HasMore = rows[:limit], true
	}
	total, err := s.repo.CountCreations(ctx, repo.CreationFilter{
		WorkspaceID: workspaceID, ResultType: resultType, ProjectID: pid,
	})
	if err != nil {
		s.log.Warn("作品计数失败，已降级", "error", err)
		total = -1
	}
	page.Total = total
	return page, nil
}
