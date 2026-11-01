// Package transport 暴露项目/文件夹/回收站 HTTP 接口。
package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Handler struct {
	svc *app.Service
	// userIDs 由身份域注入 —— 本域不该知道 JWT claims 长什么样，
	// 直接 import 身份域的 transport 会让两个领域在传输层互相纠缠。
	userIDs func(ctx context.Context) (int64, bool)
}

func NewHandler(svc *app.Service, userIDs func(ctx context.Context) (int64, bool)) *Handler {
	return &Handler{svc: svc, userIDs: userIDs}
}

// Mount 挂载路由。
//
// requireUser 与 requireWorkspace 都由外部注入：本域不该知道 JWT claims 结构，
// 也不该自己解析 X-Workspace-Id —— 那条校验属于协作域（CD-04）。
func (h *Handler) Mount(r chi.Router, requireUser func(http.Handler) http.Handler,
	requireWorkspace func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser)

		// 模板不属于任何工作区，不要求工作区上下文。
		r.Get("/project-templates", h.listTemplates)

		r.Group(func(r chi.Router) {
			r.Use(requireWorkspace)

			// ⚠️ /projects/trash 必须注册在 /projects/{projectId} 之前 ——
			// chi 虽然静态段优先，但把它写在后面容易在重构时被改成通配路由。
			r.Get("/projects/trash", h.listTrash)
			r.Get("/projects", h.list)
			r.Post("/projects", h.create)
			r.Get("/projects/{projectId}", h.get)
			r.Put("/projects/{projectId}/canvas", h.saveCanvas)
			// 空画布上的四张快捷生成卡片：只允许空画布、带版本号（与保存画布同一条乐观锁）。
			r.Post("/projects/{projectId}/template", h.applyTemplate)
			// 离线重连对账：只问"现在是第几版、谁改的"，不取画布内容 ——
			// 恢复网络后直接重发的话，离线期间同事改过时用户收到的是一个 409，
			// 而他刚看到"网络已恢复"，会以为是重连本身出了问题（NEW-19）。
			r.Get("/projects/{projectId}/canvas/state", h.canvasState)
			// Agent 画布操作的补取（AGT-10）：打开着的前端按批次重放，补不全就整页重载。
			r.Get("/projects/{projectId}/canvas/ops", h.canvasOps)
			r.Post("/projects/{projectId}/nodes/{nodeId}/resolve-inputs", h.resolveInputs)
			r.Patch("/projects/{projectId}", h.update)
			r.Delete("/projects/{projectId}", h.delete)
			r.Post("/projects/{projectId}/duplicate", h.duplicate)
			r.Post("/projects/{projectId}/restore", h.restore)
			r.Delete("/projects/{projectId}/purge", h.purge)
			// 项目文档（AGT-09）：正文不进 canvasData，文档节点只存 docId。
			h.mountDocuments(r)
			// 3D 导演台场景（docs/agent/director）：正文不进 canvasData，导演台节点只存 sceneId。
			h.mountDirectorScenes(r)

			r.Get("/folders", h.listFolders)
			r.Post("/folders", h.createFolder)
			r.Patch("/folders/{folderId}", h.renameFolder)
			r.Delete("/folders/{folderId}", h.deleteFolder)
		})
	})
}

// ---------------------------------------------------------------- DTO

type projectResp struct {
	ID          string  `json:"id"`
	WorkspaceID string  `json:"workspaceId"`
	CreatedBy   string  `json:"createdByUserId"`
	Name        string  `json:"name"`
	FolderID    *string `json:"folderId"`
	// CoverMedia 是**用户显式设置**的封面；列表与卡片要显示的是 DisplayCover。
	CoverMedia *string `json:"coverMediaId"`
	// DisplayCover = 显式封面 ?? 最近一张成功产出的图片（迁移 00030）。
	// ⚠️ 由服务端算好给出，**前端不自己拼**「有显式用显式、否则挑一张」——
	// 那条规则在三个页面各写一遍的话，早晚会有一页的封面和别处不一样。
	DisplayCover *string         `json:"displayCoverMediaId"`
	CanvasData   json.RawMessage `json:"canvasData,omitempty"`
	Version      int             `json:"version"`
	Status       string          `json:"status"`
	DeletedAt    *string         `json:"deletedAt"`
	// DaysLeft 只在回收站列表里有意义，省得前端各自按保留期再算一遍。
	DaysLeft *int `json:"daysLeft,omitempty"`
	// Origin 是项目来源（MKD-3）：null = 未记录（历史项目与非营销入口），"marketing" = 营销会话建的。
	// ⚠️ 字段一定在（null 也输出），建后不可改。
	Origin    *string `json:"origin"`
	CreatedAt string  `json:"createdAt"`
	UpdatedAt string  `json:"updatedAt"`
}

func (h *Handler) toResp(p repo.Project, withCanvas bool) projectResp {
	r := projectResp{
		// ID 对外一律 string，防 JS 精度丢失。
		ID: idStr(p.ID), WorkspaceID: idStr(p.WorkspaceID), CreatedBy: idStr(p.CreatedBy),
		Name: p.Name, Version: p.Version, Status: p.Status,
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if p.FolderID != 0 {
		s := idStr(p.FolderID)
		r.FolderID = &s
	}
	if p.CoverMedia != 0 {
		s := idStr(p.CoverMedia)
		r.CoverMedia = &s
	}
	if p.DisplayCover != 0 {
		s := idStr(p.DisplayCover)
		r.DisplayCover = &s
	}
	if p.Origin != "" {
		s := p.Origin
		r.Origin = &s
	}
	if p.DeletedAt != nil {
		s := p.DeletedAt.UTC().Format(time.RFC3339)
		r.DeletedAt = &s
		d := domain.DaysLeft(*p.DeletedAt, time.Now(), h.svc.Retention())
		r.DaysLeft = &d
	}
	if withCanvas {
		r.CanvasData = p.CanvasData
	}
	return r
}

// agentBriefResp 是列表项上的 `agent` 字段（AGT-45）：项目最近活跃的 Agent 会话。
type agentBriefResp struct {
	SessionID    string `json:"sessionId"`
	Title        string `json:"title"`
	State        string `json:"state"`
	LastActiveAt string `json:"lastActiveAt"`
}

// projectListItem 是列表项：项目 + `agent`（没有会话时为 null，**字段一定在**）。
// 单项接口（详情 / 新建 / 改名）不带这个字段 —— 那几条查询不算会话，带一个恒为 null
// 的字段会让前端误以为「这不是 Agent 项目」。
type projectListItem struct {
	projectResp
	Agent *agentBriefResp `json:"agent"`
}

func agentBrief(b *repo.AgentBrief) *agentBriefResp {
	if b == nil {
		return nil
	}
	return &agentBriefResp{
		SessionID: idStr(b.SessionID), Title: b.Title, State: b.State,
		LastActiveAt: b.LastActiveAt.UTC().Format(time.RFC3339),
	}
}

// parseKind 解析 `kind`：缺省 / all = 全部；非法值 400（筛选写错了却静默给全部，
// 用户会以为「Agent 项目」就是全部项目）。
func parseKind(raw string) (repo.ProjectKind, bool) {
	switch raw {
	case "", "all":
		return repo.KindAll, true
	case "canvas":
		return repo.KindCanvas, true
	case "agent":
		return repo.KindAgent, true
	case "marketing":
		return repo.KindMarketing, true
	}
	return "", false
}

// ---------------------------------------------------------------- handlers

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	h.page(w, r, false)
}

func (h *Handler) listTrash(w http.ResponseWriter, r *http.Request) {
	h.page(w, r, true)
}

func (h *Handler) page(w http.ResponseWriter, r *http.Request, trash bool) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	in := app.ListInput{
		Keyword: q.Get("keyword"), Limit: limit, Cursor: q.Get("cursor"), Trash: trash,
	}
	if !trash {
		kind, ok := parseKind(q.Get("kind"))
		if !ok {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "kind 非法，只能是 all / canvas / agent / marketing"))
			return
		}
		in.Kind = kind
	}
	// folderId=none 表示「未分类」。用空串表达会与「不筛选」混淆，
	// 而前端两种诉求都存在：全部项目 vs 没归类的项目。
	switch fid := q.Get("folderId"); fid {
	case "":
	case "none":
		in.NoFolder = true
	default:
		id, err := strconv.ParseInt(fid, 10, 64)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "folderId 非法"))
			return
		}
		in.FolderID = id
	}

	page, err := h.svc.List(r.Context(), ws.WorkspaceID, ws.Role, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]projectListItem, 0, len(page.Items))
	for _, p := range page.Items {
		items = append(items, projectListItem{projectResp: h.toResp(p, false), Agent: agentBrief(p.Agent)})
	}
	var next *string
	if page.NextCursor != "" {
		next = &page.NextCursor
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items": items, "nextCursor": next, "hasMore": page.HasMore,
	})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	var req struct {
		Name         string  `json:"name"`
		FolderID     *string `json:"folderId"`
		TemplateCode *string `json:"templateCode"`
	}
	if !decode(w, r, &req) {
		return
	}
	in := app.CreateInput{Name: req.Name}
	if req.FolderID != nil && *req.FolderID != "" {
		id, err := strconv.ParseInt(*req.FolderID, 10, 64)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "folderId 非法"))
			return
		}
		in.FolderID = id
	}
	if req.TemplateCode != nil {
		in.TemplateCode = *req.TemplateCode
	}
	p, err := h.svc.Create(r.Context(), ws.WorkspaceID, uid, ws.Role, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, h.toResp(p, false))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	p, err := h.svc.Get(r.Context(), ws.WorkspaceID, id, ws.Role)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h.toResp(p, true))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	var raw map[string]json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	in, err := parseUpdate(raw)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	p, err := h.svc.Update(r.Context(), ws.WorkspaceID, id, ws.Role, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h.toResp(p, false))
}

func (h *Handler) saveCanvas(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	var req struct {
		// json.RawMessage：**不要**反序列化成 map 再存回去 ——
		// 那样后端没定义的字段会被吃掉，而画布 JSON 一定会随前端长出新字段（GO-35）。
		CanvasData json.RawMessage `json:"canvasData"`
		Version    int             `json:"version"`
	}
	if !decode(w, r, &req) {
		return
	}
	userID, _ := h.userIDs(r.Context())
	res, err := h.svc.SaveCanvas(r.Context(), ws.WorkspaceID, id, userID, ws.Role,
		req.Version, req.CanvasData)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"version":   res.Version,
		"updatedAt": res.UpdatedAt.UTC().Format(time.RFC3339),
	})
}

// resolveInputs 生成前预检：把声明的输入解析成可信清单。
//
// ⚠️ 请求体里**没有 url / objectKey 字段**，这是有意的（GO-27）：
// 能接受它们的那一刻起，"服务端按可信 ID 解析"这条规则就形同虚设。
func (h *Handler) resolveInputs(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	projectID, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	var req struct {
		Operation string `json:"operation"`
		Inputs    []struct {
			Kind         string `json:"kind"`
			Order        int    `json:"order"`
			SourceNodeID string `json:"sourceNodeId"`
			Text         string `json:"text"`
			GenerationID string `json:"generationId"`
			ReferenceID  string `json:"referenceId"`
			MaterialCode string `json:"materialCode"`
			MaterialType string `json:"materialType"`
			VoiceCode    string `json:"voiceCode"`
		} `json:"inputs"`
	}
	if !decode(w, r, &req) {
		return
	}

	declared := make([]domain.DeclaredInput, 0, len(req.Inputs))
	for _, in := range req.Inputs {
		d := domain.DeclaredInput{
			Kind: domain.InputKind(in.Kind), Order: in.Order, SourceNodeID: in.SourceNodeID,
			Text: in.Text, MaterialCode: in.MaterialCode, MaterialType: in.MaterialType,
			VoiceCode: in.VoiceCode,
		}
		// ID 对外是 string（防 JS 精度丢失），进来要转回 int64。
		if in.GenerationID != "" {
			d.GenerationID, _ = strconv.ParseInt(in.GenerationID, 10, 64)
		}
		if in.ReferenceID != "" {
			d.ReferenceID, _ = strconv.ParseInt(in.ReferenceID, 10, 64)
		}
		declared = append(declared, d)
	}

	res, err := h.svc.ResolveInputs(r.Context(), ws.WorkspaceID, ws.Role, app.ResolveRequest{
		ProjectID: projectID, NodeID: chi.URLParam(r, "nodeId"),
		Operation: req.Operation, Inputs: declared,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

func (h *Handler) duplicate(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // 允许空 body：不传就用「原名 + 副本」

	p, err := h.svc.Duplicate(r.Context(), ws.WorkspaceID, uid, id, ws.Role, req.Name)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, h.toResp(p, false))
}

func (h *Handler) applyTemplate(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	var req struct {
		TemplateCode string `json:"templateCode"`
		Version      int    `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TemplateCode == "" {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "templateCode 必填"))
		return
	}
	p, err := h.svc.ApplyTemplate(r.Context(), ws.WorkspaceID, id, uid, ws.Role, req.TemplateCode, req.Version)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 带画布返回：前端要用它直接替换本地画布与版本号，再取一次就多一个往返。
	httpx.JSON(w, http.StatusOK, h.toResp(p, true))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), ws.WorkspaceID, uid, id, ws.Role); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) restore(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	p, err := h.svc.Restore(r.Context(), ws.WorkspaceID, uid, id, ws.Role)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, h.toResp(p, false))
}

func (h *Handler) purge(w http.ResponseWriter, r *http.Request) {
	ws, uid, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	if err := h.svc.Purge(r.Context(), ws.WorkspaceID, uid, id, ws.Role); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- 文件夹与模板

type folderResp struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Sort         int    `json:"sort"`
	ProjectCount int    `json:"projectCount"`
	CreatedAt    string `json:"createdAt"`
}

func toFolder(f repo.Folder) folderResp {
	return folderResp{idStr(f.ID), f.Name, f.Sort, f.ProjectCount,
		f.CreatedAt.UTC().Format(time.RFC3339)}
}

func (h *Handler) listFolders(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	list, err := h.svc.ListFolders(r.Context(), ws.WorkspaceID, ws.Role)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := make([]folderResp, 0, len(list))
	for _, f := range list {
		out = append(out, toFolder(f))
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) createFolder(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	f, err := h.svc.CreateFolder(r.Context(), ws.WorkspaceID, ws.Role, req.Name)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toFolder(f))
}

func (h *Handler) renameFolder(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "folderId")
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	f, err := h.svc.RenameFolder(r.Context(), ws.WorkspaceID, id, ws.Role, req.Name)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toFolder(f))
}

func (h *Handler) deleteFolder(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "folderId")
	if !ok {
		return
	}
	if err := h.svc.DeleteFolder(r.Context(), ws.WorkspaceID, id, ws.Role); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listTemplates(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListTemplates(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type resp struct {
		Code        string `json:"code"`
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Sort        int    `json:"sort"`
	}
	out := make([]resp, 0, len(list))
	for _, t := range list {
		out = append(out, resp{t.Code, t.Name, t.Description, t.Sort})
	}
	httpx.JSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- 小工具

func (h *Handler) ctx(w http.ResponseWriter, r *http.Request) (collabtransport.WorkspaceContext, int64, bool) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.PermissionDenied))
		return ws, 0, false
	}
	uid, ok := h.userIDs(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
		return ws, 0, false
	}
	return ws, uid, true
}

func pathID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, key+" 非法"))
		return 0, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "请求体不是合法 JSON"))
		return false
	}
	return true
}

// parseUpdate 把 PATCH /projects/{id} 的请求体解析成更新输入。
//
// ⚠️ 三态语义要用「指针的指针」表达：
// 外层 nil = 字段没出现（不改）；内层 nil = 显式传了 null（清空）。
// 只用一层指针的话，"移出文件夹" 和 "别动文件夹" 在 Go 里长得一模一样。
func parseUpdate(raw map[string]json.RawMessage) (app.UpdateInput, error) {
	var in app.UpdateInput
	if err := rejectImmutable(raw); err != nil {
		return in, err
	}
	if v, ok := raw["name"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return in, httpx.Errf(errcode.InvalidArgument, "name 非法")
		}
		in.Name = &s
	}
	var err error
	if in.FolderID, err = optionalID(raw, "folderId"); err != nil {
		return in, httpx.Errf(errcode.InvalidArgument, "folderId 非法")
	}
	if in.CoverID, err = optionalID(raw, "coverMediaId"); err != nil {
		return in, httpx.Errf(errcode.InvalidArgument, "coverMediaId 非法")
	}
	return in, nil
}

// rejectImmutable 拒绝请求体里出现建后不可改的字段（MKD-3、MKT-AC-21③）。
//
// ⚠️ 更新接口按 map 解码、只认识的键才处理，其余键**静默忽略**（B-9）：不显式拒绝的话，
// 带 origin 的请求回 200 而什么都没改，调用方会以为改成功了。哪怕传的是 null 也拒 ——
// 「清空来源」同样是在改它。
func rejectImmutable(raw map[string]json.RawMessage) error {
	if _, ok := raw["origin"]; ok {
		return httpx.Errf(errcode.InvalidArgument, "origin 建后不可修改")
	}
	return nil
}

// optionalID 解析三态的 ID 字段：字段缺失 → nil；null → 指向 nil；有值 → 指向该值。
func optionalID(raw map[string]json.RawMessage, key string) (**int64, error) {
	v, ok := raw[key]
	if !ok {
		return nil, nil
	}
	var inner *int64
	var s *string
	if err := json.Unmarshal(v, &s); err != nil {
		return nil, err
	}
	if s != nil && *s != "" {
		id, err := strconv.ParseInt(*s, 10, 64)
		if err != nil {
			return nil, err
		}
		inner = &id
	}
	return &inner, nil
}

func idStr(v int64) string { return strconv.FormatInt(v, 10) }

// canvasState 只回版本与最后编辑者，**不回画布内容**。
//
// 离线重连每次都要问一句，而画布 JSON 有几百 KB 到几 MB ——
// 顺手带上内容的话，一次网络抖动就要把整张图重新下载一遍。
func (h *Handler) canvasState(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := h.ctx(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	st, err := h.svc.CanvasState(r.Context(), ws.WorkspaceID, id, ws.Role)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	userID, _ := h.userIDs(r.Context())
	httpx.JSON(w, http.StatusOK, map[string]any{
		"version":        st.Version,
		"updatedAt":      st.UpdatedAt.UTC().Format(time.RFC3339),
		"lastEditorId":   strconv.FormatInt(st.EditorID, 10),
		"lastEditorName": st.EditorName,
		"bySelf":         app.BySelf(st, userID),
		// AGT-10：最后一次写入是真人保存（user）、Agent 的画布操作（agent）还是 MCP 写入（mcp，MCPT-09）。
		"editorKind":     st.EditorKind,
		"agentSessionId": optionalIDString(st.AgentSessionID),
		"mcpGrantId":     optionalIDString(st.McpGrantID),
	})
}
