package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 自建模型的「推荐配置」（2026-11-03 用户反馈：新建模型直接打开「启用」会被拒 ——「至少需要绑定并启用一个操作」，
// 而绑定操作、给每个操作配能力矩阵要另开抽屉逐条点）。
//
// 推荐的依据就是内置厂商模板（seed/data/providers.json）里已经在用的那套，不另写一份：
//   - 文本模型：模板的文本操作组（对话、分镜、提示词合成、反推…）；agent_chat 只在勾了「可作 Agent 推理模型」时绑；
//   - 图片 / 视频 / 音频模型：模板里**同协议同类型**的模型绑了哪些操作就推荐哪些；模板里没有这个协议时，
//     取协议能做、且模板有缺省能力规则的同类型操作（没有能力规则的操作绑了也过不了发布检查）。
// 每个操作带上模板的缺省能力规则。已经绑过的操作一律不动（用户可能改过能力矩阵）。

// RecommendResult 是一次推荐配置的结果。
type RecommendResult struct {
	Bound   []string `json:"bound"`   // 本次新绑的操作
	Skipped []string `json:"skipped"` // 已经绑过、原样保留的操作
}

func recommendedOperations(f tplFile, m catalogrepo.Model, ops []catalogrepo.Operation) []string {
	var picked []string
	add := func(op string) {
		if catalogdomain.CheckProtocolOperation(m.APIProtocol, op) != nil {
			return
		}
		if op == "agent_chat" && !m.IsAgentCapable {
			return
		}
		for _, p := range picked {
			if p == op {
				return
			}
		}
		picked = append(picked, op)
	}
	if m.Domain == "text" {
		for _, op := range f.OperationGroups["text"] {
			add(op)
		}
		return picked
	}
	for _, tm := range f.Models {
		if tm.Protocol == m.APIProtocol && tm.Domain == m.Domain {
			for _, op := range f.operations(tm) {
				add(op)
			}
		}
	}
	if len(picked) > 0 {
		return picked
	}
	sorted := append([]catalogrepo.Operation(nil), ops...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Sort < sorted[j].Sort })
	for _, o := range sorted {
		if o.Domain == m.Domain && o.Status != "disabled" && len(f.CapabilityRules[o.Code]) > 0 {
			add(o.Code)
		}
	}
	return picked
}

func (mt modelTemplates) recommend(w http.ResponseWriter, r *http.Request) {
	res, err := mt.applyRecommended(r.Context(), mt.who(r), chi.URLParam(r, "model"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

func (mt modelTemplates) applyRecommended(ctx context.Context, operator int64, code string) (RecommendResult, error) {
	res := RecommendResult{Bound: []string{}, Skipped: []string{}}
	f, err := loadProviderTemplates()
	if err != nil {
		return res, err
	}
	m, err := mt.repo.GetModel(ctx, code)
	if errors.Is(err, catalogrepo.ErrNotFound) {
		return res, httpx.Errf(errcode.NotFound, fmt.Sprintf("模型 %s 不存在", code))
	}
	if err != nil {
		return res, err
	}
	ops, err := mt.svc.ListOperations(ctx)
	if err != nil {
		return res, err
	}
	bound, err := mt.repo.ListModelOperations(ctx, m.ID)
	if err != nil {
		return res, err
	}
	has := map[string]bool{}
	for _, b := range bound {
		has[b.OperationCode] = true
	}
	picked := recommendedOperations(f, m, ops)
	if len(picked) == 0 {
		return res, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf(
			"没有可推荐的操作：协议 %s 与模型类型 %s 在内置模板里没有对应配置，请在「操作与参数」里手动绑定", m.APIProtocol, m.Domain))
	}
	for _, op := range picked {
		if has[op] {
			res.Skipped = append(res.Skipped, op)
			continue
		}
		if err := mt.svc.SaveModelOperation(ctx, operator, code, op, true, nil); err != nil {
			return res, err
		}
		for _, c := range f.CapabilityRules[op] {
			if err := mt.svc.SaveCapability(ctx, operator, code, op, catalogdomain.Capability{
				Role: catalogdomain.InputRole(c.Role), MinCount: c.Min, MaxCount: c.Max,
			}); err != nil {
				return res, err
			}
		}
		res.Bound = append(res.Bound, op)
	}
	return res, nil
}
