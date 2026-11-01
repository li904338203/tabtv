package wiring

// 营销会话给推理模型看商品图（MKT-T-19）：目录域的能力矩阵 + 媒体域的缩略图。

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	providerapp "github.com/li904338203/tabtv/backend/internal/provider/app"
)

// maxReasoningImageBytes 单张内联图的上限：640 档的 PNG / JPEG 缩完只有几十到几百 KB；
// 缩不了的格式（WebP / GIF 原样返回）超过它就不内联 —— 几 MB 的 base64 每次调用都重发一遍不值。
const maxReasoningImageBytes = 1 << 20

// reasoningImageMIME 是两家推理协议都收的图片类型。
var reasoningImageMIME = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true,
}

// AgentSceneImages 实现 agentapp.SceneImages。
//
// ⚠️ 一律 base64 内联，不发签名外链（MKT-AC-05，dev/01 R-3）：推理请求会进 provider_request_log 与上游日志，
// 外链在有效期内拿到日志的人就能直接取图；内联的 base64 在请求日志里由 platform/logredact 摘要化。
type AgentSceneImages struct {
	Catalog *catalogapp.Service
	Media   *mediaapp.Service
	// Width 缩略图档位（SEEAITV_MARKETING_REASONING_IMAGE_WIDTH，只接受 160 / 320 / 640）。
	Width int
}

// ReasoningImageLimit 取推理模型在 agent_chat 上 image 角色的上限（与报价同一份能力矩阵，
// agent_chat 未声明时继承 text2text）。查不到 / 未声明 → 0。
func (a AgentSceneImages) ReasoningImageLimit(ctx context.Context, modelCode string) (int, error) {
	if a.Catalog == nil || modelCode == "" {
		return 0, nil
	}
	max, found, err := a.Catalog.InputLimit(ctx, modelCode, providerapp.OpAgentChat, catalogdomain.RoleImage)
	if err != nil || !found {
		return 0, err
	}
	return max, nil
}

// InlineImage 按工作区取缩略图并内联成 data: 地址（归属条件在媒体域的 SQL 里）。
func (a AgentSceneImages) InlineImage(ctx context.Context, workspaceID, mediaID int64) (string, error) {
	if a.Media == nil {
		return "", fmt.Errorf("媒体服务未接入")
	}
	rc, m, err := a.Media.Download(ctx, workspaceID, mediaID, a.Width)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	if !reasoningImageMIME[m.ContentType] {
		return "", fmt.Errorf("媒体 %d 不是推理模型能看的图片（%s）", mediaID, m.ContentType)
	}
	b, err := io.ReadAll(io.LimitReader(rc, maxReasoningImageBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxReasoningImageBytes {
		return "", fmt.Errorf("媒体 %d 缩略后仍超过 %d 字节，不内联", mediaID, maxReasoningImageBytes)
	}
	return "data:" + m.ContentType + ";base64," + base64.StdEncoding.EncodeToString(b), nil
}

// NewAgentViewMediaTool 装出 view_media（只在营销会话下发，domain.toolSpecs 的 Scenes）。
func NewAgentViewMediaTool(repo *agentrepo.Repo, images agentapp.SceneImages) agentapp.Tool {
	return agenttools.ViewMedia{Sessions: repo, Images: images}
}

var _ agentapp.SceneImages = AgentSceneImages{}
