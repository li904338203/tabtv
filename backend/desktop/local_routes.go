package desktop

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 本机身份的三个只读 / 自助接口（TT-T-15）。
//
// 来源系统里它们分属身份域（/auth/me、/auth/profile）与协作域（/workspaces），挂那两个域的整组路由的话
// 登录、注册、成员、邀请这些被排除的入口会一起出现。这里只按本机身份回答前端真正要的三件事。
func mountLocalRoutes(r chi.Router, id *Identity, pool *pgxpool.Pool) {
	// 工作区列表：只有一个（前端 stores/workspace 的形状）。
	r.Get("/workspaces", func(w http.ResponseWriter, req *http.Request) {
		var name string
		if err := pool.QueryRow(req.Context(), `select name from workspace where id = $1`, id.WorkspaceID).Scan(&name); err != nil {
			httpx.Fail(w, req, httpx.Wrap(errcode.Internal, err))
			return
		}
		writeJSON(w, []map[string]any{{
			"id": itoa(id.WorkspaceID), "name": name, "kind": "personal", "role": "owner",
			"payerAccountId": itoa(id.PayerID),
		}})
	})
	// 当前用户。
	r.Get("/auth/me", func(w http.ResponseWriter, req *http.Request) {
		var nickname string
		var off bool
		if err := pool.QueryRow(req.Context(),
			`select nickname, is_ai_watermark_off from app_user where id = $1`, id.UserID).Scan(&nickname, &off); err != nil {
			httpx.Fail(w, req, httpx.Wrap(errcode.Internal, err))
			return
		}
		writeJSON(w, map[string]any{"id": itoa(id.UserID), "nickname": nickname, "avatar": nil,
			"phone": nil, "email": nil, "isAiWatermarkOff": off})
	})
	// 水印开关（设置 → 常规，TT-F-18）。字段是「关闭标志」：true = 不加 AI 标识。
	r.Put("/auth/profile", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			IsAiWatermarkOff *bool `json:"isAiWatermarkOff"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.IsAiWatermarkOff == nil {
			httpx.Fail(w, req, httpx.Errf(errcode.InvalidArgument, "缺少 isAiWatermarkOff"))
			return
		}
		if _, err := pool.Exec(req.Context(), `update app_user set is_ai_watermark_off = $2, updated_at = now() where id = $1`,
			id.UserID, *body.IsAiWatermarkOff); err != nil {
			httpx.Fail(w, req, httpx.Wrap(errcode.Internal, err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
