package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 设置 → 模型与厂商的「模板模式」（TT-T-18，TT-F-26）。
//
// 模板就是启动时由 providers.json 写进库的厂商与模型（provider_templates.go）。库里没有「这一行来自模板」的标记，
// 所以按 providers.json 的 code 清单辨认 —— 用户在高级模式里新建的厂商 / 模型不会出现在这里。
//
// 启用 = 写 Key（可选）→ 启用厂商 → 逐个启用模板里属于它的模型。每个模型的启用照常过目录域的发布检查
// （与高级模式「启用」同一条路），有阻塞的不启用，并把原因原样带回。
// ⚠️ 不是一个大事务：目录服务的每次保存各自落库（与高级模式一致）。部分模型启用失败不回滚已启用的 ——
// 与写作设定「成功几份落几份」同一条取舍：失败的说清原因，用户补齐后再点一次即可（已启用的会跳过）。

type templateModelView struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Domain    string `json:"domain"`
	Installed bool   `json:"installed"`
	Status    string `json:"status"`
}

type templateView struct {
	Code       string              `json:"code"`
	Name       string              `json:"name"`
	BaseURL    string              `json:"baseUrl"`
	AuthType   string              `json:"authType"`
	HeaderName string              `json:"headerName,omitempty"`
	Installed  bool                `json:"installed"`
	Status     string              `json:"status"`
	HasAPIKey  bool                `json:"hasApiKey"`
	IsReady    bool                `json:"isReady"`
	Version    int                 `json:"version"`
	Models     []templateModelView `json:"models"`
}

type templateModelResult struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Skipped  bool   `json:"skipped,omitempty"` // 本来就是启用的
	Reason   string `json:"reason,omitempty"`
	Blockers any    `json:"blockers,omitempty"`
}

type modelTemplates struct {
	svc  *catalogapp.Service
	repo *catalogrepo.Repo
	who  func(*http.Request) int64
}

func mountModelTemplates(r chi.Router, mt modelTemplates, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/model-templates", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("model:read")).Get("/", mt.list)
		r.With(requirePerm("model:write")).Post("/{code}/enable", mt.enable)
		// 给自建模型一键绑定推荐的操作与能力规则（见 model_recommend.go）。
		r.With(requirePerm("model:write")).Post("/recommend-bindings/{model}", mt.recommend)
	})
}

func (mt modelTemplates) list(w http.ResponseWriter, r *http.Request) {
	out, err := mt.views(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": out})
}

func (mt modelTemplates) views(ctx context.Context) ([]templateView, error) {
	f, err := loadProviderTemplates()
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]templateView, 0, len(f.Providers))
	for _, p := range f.Providers {
		v := templateView{Code: p.Code, Name: p.Name, BaseURL: p.BaseURL, AuthType: p.AuthType,
			HeaderName: p.HeaderName, Models: []templateModelView{}}
		if row, err := mt.repo.GetProvider(ctx, p.Code); err == nil {
			v.Installed, v.Status, v.HasAPIKey, v.IsReady, v.Version = true, row.Status, row.APIKeyCipher != "", row.IsReady(), row.Version
			v.Name, v.BaseURL = row.Name, row.BaseURL
		} else if !errors.Is(err, catalogrepo.ErrNotFound) {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		for _, m := range f.Models {
			if m.Provider != p.Code {
				continue
			}
			mv := templateModelView{Code: m.Code, Name: m.Name, Domain: m.Domain}
			if row, err := mt.repo.GetModel(ctx, m.Code); err == nil {
				mv.Installed, mv.Status, mv.Name = true, row.Status, row.Name
			} else if !errors.Is(err, catalogrepo.ErrNotFound) {
				return nil, httpx.Wrap(errcode.Internal, err)
			}
			v.Models = append(v.Models, mv)
		}
		out = append(out, v)
	}
	return out, nil
}

func (mt modelTemplates) enable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey *catalogapp.SecretField `json:"apiKey"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "请求体不是合法 JSON"))
			return
		}
	}
	res, err := mt.enableTemplate(r.Context(), mt.who(r), chi.URLParam(r, "code"), req.APIKey)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

func (mt modelTemplates) enableTemplate(ctx context.Context, operatorID int64, code string,
	key *catalogapp.SecretField) (map[string]any, error) {
	f, err := loadProviderTemplates()
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	known := false
	for _, p := range f.Providers {
		known = known || p.Code == code
	}
	if !known {
		return nil, httpx.Errf(errcode.NotFound, "没有这个厂商模板："+code)
	}
	p, err := mt.repo.GetProvider(ctx, code)
	if err != nil {
		return nil, httpx.Errf(errcode.NotFound, "模板厂商还没有写入，请重启应用后再试")
	}
	// 厂商：沿用库里的全部字段（用户可能在高级模式里改过超时），只换 Key 与状态。
	// Key 缺省 = 保留；没有 Key 又需要认证时，SaveProvider 会拒绝启用并说清楚。
	if err := mt.svc.SaveProvider(ctx, operatorID, catalogapp.ProviderInput{
		Code: p.Code, Name: p.Name, AdapterType: p.AdapterType, BaseURL: p.BaseURL,
		AuthType: p.AuthType, APIKey: key, Headers: p.Headers, TestPath: p.TestPath,
		ConnectTimeoutMS: p.ConnectTimeoutMS, RequestTimeoutMS: p.RequestTimeoutMS,
		IdleTimeoutMS: p.IdleTimeoutMS, MaxRetries: p.MaxRetries, MaxConcurrent: p.MaxConcurrent,
		Status: "enabled", Version: p.Version,
	}); err != nil {
		return nil, err
	}

	results := []templateModelResult{}
	enabled := 0
	for _, tm := range f.Models {
		if tm.Provider != code {
			continue
		}
		res := templateModelResult{Code: tm.Code, Name: tm.Name}
		m, err := mt.repo.GetModel(ctx, tm.Code)
		if err != nil {
			res.Reason = "模型还没有写入，请重启应用后再试"
			results = append(results, res)
			continue
		}
		res.Name = m.Name
		if m.Status == "enabled" {
			res.Enabled, res.Skipped = true, true
			enabled++
			results = append(results, res)
			continue
		}
		// ⚠️ 照库里的值整条回填：保存是整条替换，漏一个字段（提交路径、输入上限、排序）就会被清空。
		// 视频能力、Agent 标记、会员门槛传 nil = 保留。
		err = mt.svc.SaveModel(ctx, operatorID, catalogapp.ModelInput{
			Code: m.Code, Name: m.Name, ProviderCode: code, Domain: m.Domain, SubType: m.SubType,
			UpstreamModelCode: m.UpstreamModelCode, APIProtocol: m.APIProtocol,
			SubmitPath: m.SubmitPath, QueryPath: m.QueryPath, EstSeconds: m.EstSeconds,
			MaxInputChars: m.MaxInputChars, Status: "enabled", Sort: m.Sort,
		})
		if err != nil {
			res.Reason = err.Error()
			var be *httpx.BizError
			if errors.As(err, &be) && be.Details != nil {
				res.Blockers = be.Details["blockers"]
			}
		} else {
			res.Enabled = true
			enabled++
		}
		results = append(results, res)
	}
	return map[string]any{"provider": code, "enabledCount": enabled, "models": results}, nil
}
