package app

import (
	"context"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// InputLimit 取某模型某操作在某输入角色上的上限（营销会话按模型上限选商品图，MKT-T-20）。
//
// ⚠️ 与报价读**同一份**能力矩阵（capabilitiesFor，含推理操作的继承）：另读一份的话，
// 「卡片上说最多 4 张」与「受理时能力矩阵说最多 3 张」会对不上，而两边各自看都没错。
// found=false：模型 × 操作查不到，或该操作没声明能力矩阵 —— 调用方应交给报价去说明原因，不猜上限。
// 声明了矩阵但没有这个角色 = 不收这种输入，返回 (0, true)。
func (s *Service) InputLimit(ctx context.Context, modelCode, operationCode string, role domain.InputRole) (
	max int, found bool, err error) {
	facts, err := s.repo.LoadQuoteFacts(ctx, modelCode, operationCode)
	if err != nil {
		return 0, false, httpx.Wrap(errcode.Internal, err)
	}
	if !facts.Found {
		return 0, false, nil
	}
	caps, err := s.capabilitiesFor(ctx, facts.ModelID, operationCode)
	if err != nil {
		return 0, false, err
	}
	if len(caps) == 0 {
		return 0, false, nil
	}
	c, ok := domain.NewCapabilitySet(caps)[role]
	if !ok {
		return 0, true, nil
	}
	return c.MaxCount, true, nil
}
