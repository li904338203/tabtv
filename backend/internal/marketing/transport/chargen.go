package transport

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/marketing/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 生成角色（MKT-T-35；dev/05 MKD-28、MKD-32）。
//
// ⚠️ **路由放宽 + 处理器收紧**（同渲染端点，CD-59）：一个端点里免费的 dryRun 报价与扣费的真生成并存，
// 而 `dryRun` 在请求体里、中间件看不到。所以这条路由放在**只装工作区上下文**（wsCtx）的独立分组，
// 处理器里：dryRun 只要 view（任何成员都有）、真生成 `Allows(generate)`，且判定在**调用服务之前**。
// ⚠️ 不能用 `r.With` 去放宽 MountLibrary 那一组的默认闸：With 是追加在组中间件之后的，
// 默认闸（POST → edit）先跑就已经拒了。

// SetCharacterGen 注入生成角色服务与会员等级解析器。
//
// ⚠️ memberLevel 必须是 cmd/api 里注入 `POST /generations` 的**同一个**函数（付费主体 → 会员等级）：
// 各写一份的话「这里说能用、那边说等级不够」迟早出现。
func (h *Handler) SetCharacterGen(gen *app.CharacterGen, memberLevel func(*http.Request) int64) {
	h.charGen, h.memberLevel = gen, memberLevel
}

// MountCharacterGen 挂载 `POST /marketing/characters/generate`。未注入 SetCharacterGen 时不挂。
// 操作者取自 SetLibrary 注入的 userIDs（与库接口同一个登录态解析）。
func (h *Handler) MountCharacterGen(r chi.Router, requireUser, wsCtx func(http.Handler) http.Handler) {
	if h.charGen == nil {
		return
	}
	r.Group(func(r chi.Router) {
		r.Use(h.gate)
		r.Use(requireUser)
		r.Use(wsCtx)
		r.Post("/marketing/characters/generate", h.generateCharacter)
	})
}

type characterGenReq struct {
	ModelCode         string   `json:"modelCode"`
	Description       string   `json:"description"`
	StyleCode         string   `json:"styleCode"`
	ReferenceMediaIDs []string `json:"referenceMediaIds"`
	Count             int      `json:"count"`
	DryRun            bool     `json:"dryRun"`
}

type characterGenQuoteResp struct {
	Operation     string  `json:"operation"`
	Credits       int     `json:"credits"`
	Unit          string  `json:"unit"`
	Blocked       bool    `json:"blocked"`
	BlockedReason *string `json:"blockedReason,omitempty"`
	UserFixable   *bool   `json:"userFixable,omitempty"`
}

type characterGenAcceptResp struct {
	Operation    string `json:"operation"`
	GenerationID string `json:"generationId"`
	Credits      int    `json:"credits"`
	Replayed     bool   `json:"replayed"`
}

func (h *Handler) generateCharacter(w http.ResponseWriter, r *http.Request) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		// 工作区上下文没装（中间件顺序写反）时一律拒，不是放行（CD-59）。
		httpx.Fail(w, r, httpx.Err(errcode.NotWorkspaceMember))
		return
	}
	var req characterGenReq
	dec := json.NewDecoder(io.LimitReader(r.Body, maxLibraryBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("请求体非法：%v", err)))
		return
	}
	// ⚠️ 真生成要 generate，dryRun 不要（view 由工作区成员资格本身保证）。
	// 判定在**调用服务之前**：放进服务里的话「校验 → 报价 → 扣费」的顺序要为权限再让一次路。
	if !req.DryRun && !collabtransport.Allows(r, collabdomain.ActionGenerate) {
		httpx.Fail(w, r, collabtransport.DenyAction(r, collabdomain.ActionGenerate))
		return
	}
	refs := make([]int64, 0, len(req.ReferenceMediaIDs))
	for _, s := range req.ReferenceMediaIDs {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			id = 0 // 交给领域校验报 referenceMediaIds[i]，与其它问题一次报完
		}
		refs = append(refs, id)
	}
	actor := app.CharacterGenActor{WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID}
	if h.userIDs != nil {
		actor.UserID = h.userIDs(r)
	}
	if h.memberLevel != nil {
		actor.MemberLevelID = h.memberLevel(r)
	}
	res, err := h.charGen.Generate(r.Context(), actor, app.CharacterGenInput{
		ModelCode: req.ModelCode, Description: req.Description, StyleCode: req.StyleCode,
		ReferenceMediaIDs: refs, Count: req.Count, DryRun: req.DryRun,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if res.Quote != nil {
		out := characterGenQuoteResp{Operation: res.Operation, Credits: res.Quote.Credits, Unit: res.Quote.Unit,
			Blocked: res.Quote.Blocked}
		if res.Quote.Blocked {
			reason, fixable := res.Quote.BlockedReason, res.Quote.UserFixable
			out.BlockedReason, out.UserFixable = &reason, &fixable
		}
		httpx.JSON(w, http.StatusOK, out)
		return
	}
	// 重放回 200、新受理回 201（同 POST /generations）。
	status := http.StatusCreated
	if res.Accepted.Replayed {
		status = http.StatusOK
	}
	httpx.JSON(w, status, characterGenAcceptResp{Operation: res.Operation,
		GenerationID: idStr(res.Accepted.GenerationID), Credits: res.Accepted.Credits,
		Replayed: res.Accepted.Replayed})
}
