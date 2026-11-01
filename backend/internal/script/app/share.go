package app

import (
	"context"
	"encoding/json"
	"errors"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MediaCopier 由媒体域实现：把一份媒体复制进另一个工作区。
//
// ⚠️ 端口定义在这里而不是协作域：协作域只知道"导入一份资源"，
// 它不该知道资产带图这件事。反过来把媒体复制写进协作域的话，
// 下一种可导入资源（比如带封面的时间线）又要在那里再加一段 ——
// 协作域会慢慢变成一个知道所有业务域内部结构的地方。
type MediaCopier interface {
	CopyToWorkspace(ctx context.Context, mediaID, targetWorkspaceID, actorUserID int64) (int64, error)
}

// AssetShare 让资产可以被共享与导入（NEW-16、GAP-11）。
type AssetShare struct {
	svc   *Service
	media MediaCopier
}

func NewAssetShare(s *Service, media MediaCopier) *AssetShare {
	return &AssetShare{svc: s, media: media}
}

func (a *AssetShare) ResourceType() collabdomain.ResourceType {
	return collabdomain.ResourceScriptAsset
}

// Describe 取资产的共享视图。
func (a *AssetShare) Describe(ctx context.Context, id int64, detail bool) (collab.ResourceView, error) {
	as, err := a.svc.repo.GetAssetForShare(ctx, id)
	if errors.Is(err, repo.ErrNotFound) {
		return collab.ResourceView{}, collab.ErrResourceGone
	}
	if err != nil {
		return collab.ResourceView{}, err
	}
	v := collab.ResourceView{
		ID: as.ID, WorkspaceID: as.WorkspaceID, Name: as.Name,
		Kind: as.Type, UpdatedAt: as.UpdatedAt,
	}
	if detail {
		// ⚠️ 明确列字段。尤其**不给 mediaObjectId**：那是源工作区的媒体，
		// 受让方拿着它去调下载端点一律 401（媒体按工作区卡归属），
		// 而界面上看到的是一张裂图 —— 他会以为图坏了。
		// 要用这张图只有一条路：导入，导入会真的复制一份到他自己的工作区。
		v.Detail, err = json.Marshal(struct {
			Type        string          `json:"assetType"`
			Description string          `json:"description"`
			Profile     json.RawMessage `json:"profile"`
			Status      string          `json:"status"`
			HasImage    bool            `json:"hasImage"`
		}{
			Type: as.Type, Description: as.Description, Profile: as.Profile,
			Status: as.Status, HasImage: as.MediaObjectID != 0,
		})
		if err != nil {
			return collab.ResourceView{}, err
		}
	}
	return v, nil
}

// ImportTo 把一份共享资产复制进目标工作区。
//
// ⚠️ 副本的 source 一律记 **upload**，不继承源资产的 ai/template：
// source 表示"这份描述是怎么来的"，而下游据此决定要不要在 reconcile 里
// 覆盖它 —— 记成 ai 的话，受让方一跑自动识别，这份辛苦导入的档案
// 就被模型重新生成的内容盖掉了，而他没做任何删除动作
// （「reconcile 不删用户的工作：已 ready 或上传来源的不覆盖描述」）。
//
// ⚠️ **不建 shot_asset_ref**：导入来的资产进的是工作区资产库，
// 不属于任何脚本。自动挂到某个脚本上会让它参与那个脚本的自动归并 ——
// 而"自动识别只在本脚本内归并"正是 AV-20 要守的那条线。
func (a *AssetShare) ImportTo(ctx context.Context, targetWorkspaceID,
	resourceID, actorUserID int64) (int64, error) {
	src, err := a.svc.repo.GetAssetForShare(ctx, resourceID)
	if errors.Is(err, repo.ErrNotFound) {
		return 0, collab.ErrResourceGone
	}
	if err != nil {
		return 0, err
	}
	if src.WorkspaceID == targetWorkspaceID {
		// 共享给自己所在工作区的资源被导入会凭空多出一份同名资产，
		// 而用户完全看不出两者的区别。
		return 0, httpx.Errf(errcode.InvalidArgument, "这份资产已经在当前工作区里")
	}

	// 先复制图：复制失败就整件事失败，不落一个"有描述没有图"的半成品 ——
	// 那种资产看起来是好的，直到用户拿它去出图才发现没有参考。
	var mediaID int64
	if src.MediaObjectID != 0 && a.media != nil {
		if mediaID, err = a.media.CopyToWorkspace(ctx, src.MediaObjectID,
			targetWorkspaceID, actorUserID); err != nil {
			return 0, err
		}
	}

	newID := a.svc.nextID()
	status := "pending"
	if mediaID != 0 {
		status = "ready"
	}
	if err := a.svc.repo.InsertAsset(ctx, repo.Asset{
		ID: newID, WorkspaceID: targetWorkspaceID, Type: src.Type,
		Name: src.Name, Description: src.Description, Profile: src.Profile,
		MediaObjectID: mediaID, Source: "upload", Status: status,
	}); err != nil {
		return 0, err
	}
	return newID, nil
}
