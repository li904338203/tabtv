package app

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// CameraPrompt 按节点上选的运镜（`params.camera`）取运镜目录里这一项的提示词，没选时返回空串。
//
// ⚠️ 选了一个**已下线或不存在**的运镜时报错（扣费之前），而不是静默按没选处理：
// 输入框里那个标签还在，生成出来却没有那个镜头运动 —— 就是「选了等于没选」。与风格、特效素材码查不到时同一个处理。
func (s *Service) CameraPrompt(ctx context.Context, params json.RawMessage) (string, error) {
	code, name := domain.CameraSelection(params)
	if code == "" {
		return "", nil
	}
	prompts, err := s.repo.MaterialPromptsByCode(ctx, domain.MaterialCamera, []string{code})
	if err != nil {
		return "", httpx.Wrap(errcode.Internal, err)
	}
	prompt, ok := prompts[code]
	if !ok {
		label := name
		if label == "" {
			label = code
		}
		return "", httpx.Errf(errcode.InvalidArgument, "运镜「"+label+"」已下线，请在运镜广场重新选择")
	}
	return strings.TrimSpace(prompt), nil
}
