package app

import (
	"context"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// DeriveShareImport 是跨工作区导入时的派生种类。
const DeriveShareImport = "share_import"

// CopyToWorkspace 把一份媒体复制进另一个工作区，返回新媒体 ID。
//
// ⚠️ **真的复制字节**，不是新建一行指向同一个 object_key。
// 共用 key 的话，源工作区清理素材会让受让方那边的图凭空裂掉 ——
// 两边都不报错，而受让方甚至不知道那张图原来是别人的
// （与 00019 里「派生块是独立的字节，存在不依赖源图」是同一条）。
//
// ⚠️ 源侧**不带 workspace_id 条件**：调用方（共享域）已经按 resource_share
// 校验过授权，受让方本来就不在源工作区里。正因为如此，这个方法不能被直接
// 暴露成接口 —— 它信任调用方已经完成了归属判定。
func (s *Service) CopyToWorkspace(ctx context.Context, mediaID,
	targetWorkspaceID, actorUserID int64) (int64, error) {
	if s.store == nil {
		return 0, httpx.Errf(errcode.Internal, "对象存储未配置")
	}
	src, err := s.repo.Get(ctx, mediaID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return 0, httpx.Errf(errcode.NotFound, "源媒体已不存在")
		}
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	// 已经复制过就把上次那份还回去：导入一次失败重试、或者用户点了两下，
	// 都不该在目标工作区里堆出两份一模一样的字节。
	if existing, err := s.repo.ListDerived(ctx, targetWorkspaceID, src.ID,
		DeriveShareImport, ""); err == nil && len(existing) > 0 {
		return existing[0].ID, nil
	}

	body, _, err := s.Open(ctx, src.ID)
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }()

	up, err := s.Upload(ctx, UploadInput{
		WorkspaceID: targetWorkspaceID, UserID: actorUserID,
		// 与上传参考同一个 purpose：导入来的图要能立刻出现在"我的素材"里
		// 并作为下一次生成的输入。另起一个 purpose 的话，
		// 每一处按 purpose 过滤的地方都要再加一个分支才认得它。
		Purpose:      "upload_reference",
		DeclaredMIME: src.ContentType, Size: src.SizeBytes,
		// ExpectKind 留空：这份字节在**第一次上传时**已经嗅探并校验过大类，
		// 这里再限定一次只会把当时合法收下的格式挡在门外（比如一段被共享的
		// 参考视频），而用户得到的提示会是"此处只接受图片文件"—— 与他正在
		// 做的事完全对不上。Upload 仍会重新嗅探 MIME 并按真实内容落库。
		Body:   body,
		Derive: &DeriveInfo{SourceMediaID: src.ID, Kind: DeriveShareImport},
	})
	if err != nil {
		return 0, err
	}
	return up.ID, nil
}
