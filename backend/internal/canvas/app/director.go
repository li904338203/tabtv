package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	director "github.com/li904338203/tabtv/backend/internal/director/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 3D 导演台场景（docs/agent/director，DRT-02）
//
// 场景归画布域存（DRD-11）：它从属于 project，而复制项目必须与建项目同一个事务（DRD-30）。
// 场景的形状、校验、指纹都在 director/domain（纯函数），这里只做权限、归属、乐观锁与事件。
//
// 角色：读要 view、写要 edit。路由层的 GuardByMethod 已经按 HTTP 方法挡了一道，
// 这里再判一次是因为 Agent / MCP 的搭景写入（DRT-19 / DRT-29）不经过路由。

// DirectorActor 是写场景的人：真人、Agent（AgentSessionID 非 0）或外部 MCP 客户端（McpGrantID 非 0）。
// 与文档同形，source() 决定 `director_scene.source`。
type DirectorActor = DocumentActor

// CreateDirectorSceneInput 新建场景的输入。
type CreateDirectorSceneInput struct {
	CanvasNodeID string
	// SourceSceneID 非 0 时复制那份场景（粘贴、复制出来的节点）。
	SourceSceneID int64
}

// DirectorSceneView 是场景与它的媒体记录（按时间倒序，含已隐藏的）。
type DirectorSceneView struct {
	Scene   repo.DirectorScene
	Records []repo.DirectorMedia
}

// preparedScene 是校验通过、算好指纹与媒体引用的正文。
type preparedScene struct {
	raw         json.RawMessage
	fingerprint string
	mediaIDs    []string
}

// CreateDirectorScene 为画布节点建场景，按（项目，画布节点）幂等（DRD-17）。
// 第二个返回值是「这次真的新建了」：已有时返回已有那份与 false（HTTP 200，新建是 201）。
func (s *Service) CreateDirectorScene(ctx context.Context, ws, projectID int64, actor DirectorActor,
	role collab.Role, in CreateDirectorSceneInput) (DirectorSceneView, bool, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return DirectorSceneView{}, false, err
	}
	nodeID := in.CanvasNodeID
	if strings.TrimSpace(nodeID) == "" || utf8.RuneCountInString(nodeID) > director.MaxIDChars {
		return DirectorSceneView{}, false, httpx.Errf(errcode.InvalidArgument,
			"canvasNodeId 必填，且不超过 64 个字符")
	}
	if err := s.projectWritable(ctx, ws, projectID); err != nil {
		return DirectorSceneView{}, false, directorProjectErr(err)
	}
	// 已有就直接返回：同一节点重复建（网络重试、两个标签页）收敛到同一份，不必再算一遍正文。
	if existing, err := s.repo.GetDirectorSceneByNode(ctx, ws, projectID, nodeID); err == nil {
		view, err := s.directorView(ctx, existing)
		return view, false, err
	} else if !errors.Is(err, repo.ErrNotFound) {
		return DirectorSceneView{}, false, httpx.Wrap(errcode.Internal, err)
	}

	body := director.DefaultScene()
	var fork *repo.DirectorFork
	if in.SourceSceneID != 0 {
		src, err := s.directorSource(ctx, ws, in.SourceSceneID)
		if err != nil {
			return DirectorSceneView{}, false, err
		}
		body = src
		// 截图记录随正文一起复制（DRT-03）：副本节点 params 里镜像的截图，要有副本自己的记录保护着。
		fork = &repo.DirectorFork{SourceSceneID: in.SourceSceneID, NextID: s.nextID}
	}
	p, err := s.prepareDirectorScene(ctx, ws, body)
	if err != nil {
		return DirectorSceneView{}, false, err
	}
	sc, created, err := s.repo.CreateDirectorScene(ctx, repo.DirectorScene{
		ID: s.nextID(), WorkspaceID: ws, ProjectID: projectID, CanvasNodeID: nodeID,
		Scene: p.raw, SchemaVersion: director.CurrentSchemaVersion, RenderFingerprint: p.fingerprint,
		MediaIDs: p.mediaIDs, Source: actor.source(), EditorID: actor.UserID,
		AgentSessionID: actor.AgentSessionID, McpGrantID: actor.McpGrantID,
	}, fork)
	if err != nil {
		return DirectorSceneView{}, false, wrapDirectorRepo(err)
	}
	view, err := s.directorView(ctx, sc)
	return view, created, err
}

// directorSource 取分叉的源场景正文。
//
// ⚠️ 源场景必须在**同一工作区**（查询本身带工作区条件），且所在项目可读：回收站里的也算 ——
// 复制一份出来不会改变源项目的任何东西。不存在与不是你的回同一个 404。
//
// ⚠️ 正文先压成紧凑格式再用：从 jsonb 读出来的文本在每个冒号与逗号后都带空格，
// 一份接近 512 KiB 上限的场景读出来就会超限，复制时被误判成 41301。
func (s *Service) directorSource(ctx context.Context, ws, sourceID int64) (json.RawMessage, error) {
	src, err := s.repo.GetDirectorSceneInWorkspace(ctx, ws, sourceID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, httpx.Errf(errcode.NotFound, "要复制的场景不存在")
		}
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	if err := s.projectReadable(ctx, ws, src.ProjectID); err != nil {
		if isNotFound(err) {
			return nil, httpx.Errf(errcode.NotFound, "要复制的场景不存在")
		}
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, src.Scene); err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return buf.Bytes(), nil
}

// GetDirectorScene 取场景正文与媒体记录。回收站里的项目也能读。
func (s *Service) GetDirectorScene(ctx context.Context, ws, projectID, sceneID int64, role collab.Role) (
	DirectorSceneView, error) {
	if err := requireRole(role, collab.ActionView); err != nil {
		return DirectorSceneView{}, err
	}
	if err := s.projectReadable(ctx, ws, projectID); err != nil {
		return DirectorSceneView{}, err
	}
	sc, err := s.repo.GetDirectorScene(ctx, ws, projectID, sceneID)
	if err != nil {
		return DirectorSceneView{}, wrapDirectorRepo(err)
	}
	return s.directorView(ctx, sc)
}

// ListDirectorScenes 列出项目的场景摘要（不带正文）。
func (s *Service) ListDirectorScenes(ctx context.Context, ws, projectID int64, role collab.Role) (
	[]repo.DirectorSceneSummary, error) {
	if err := requireRole(role, collab.ActionView); err != nil {
		return nil, err
	}
	if err := s.projectReadable(ctx, ws, projectID); err != nil {
		return nil, err
	}
	list, err := s.repo.ListDirectorSceneSummaries(ctx, ws, projectID)
	return list, wrapRepo(err)
}

// UpdateDirectorScene 乐观锁保存场景正文。
//
// ⚠️ 冲突返回 40902 并带上服务端当前版本与"是谁改的"（同文档，NEW-19）：Agent 搭景与网页编辑
// 写的是同一份正文，静默覆盖的话 Agent 刚摆好的走位会被用户一次自动保存抹掉。
//
// 顺序：角色 → 版本 → 正文与媒体（不碰项目）→ 项目可写 → 写入 → 事件。
// 坏正文先拒，不必为它查库；项目可写放在写入之前，回收站里的项目报 40905 而不是 404。
func (s *Service) UpdateDirectorScene(ctx context.Context, ws, projectID, sceneID int64, actor DirectorActor,
	role collab.Role, version int, raw json.RawMessage) (repo.DirectorScene, error) {
	if err := requireRole(role, collab.ActionEdit); err != nil {
		return repo.DirectorScene{}, err
	}
	if version < 1 {
		// 不接受 0：那是"我没读过服务端版本"的意思，等于要求无条件覆盖。
		return repo.DirectorScene{}, httpx.Errf(errcode.InvalidArgument, "version 必须来自服务端，不能为 0")
	}
	p, err := s.prepareDirectorScene(ctx, ws, raw)
	if err != nil {
		return repo.DirectorScene{}, err
	}
	if err := s.projectWritable(ctx, ws, projectID); err != nil {
		return repo.DirectorScene{}, directorProjectErr(err)
	}
	saved, err := s.repo.UpdateDirectorScene(ctx, ws, projectID, sceneID, version, repo.DirectorSceneWrite{
		Scene: p.raw, SchemaVersion: director.CurrentSchemaVersion, RenderFingerprint: p.fingerprint,
		MediaIDs: p.mediaIDs, Source: actor.source(), EditorID: actor.UserID,
		AgentSessionID: actor.AgentSessionID, McpGrantID: actor.McpGrantID,
	})
	if errors.Is(err, repo.ErrVersionConflict) {
		if saved.Version == version {
			// 版本对得上却没改到：项目在「查可写」与「写入」之间被删进了回收站。
			// 说成冲突的话前端「载入最新」之后拿到的还是同一个版本，再存还是冲突。
			if err := s.projectWritable(ctx, ws, projectID); err != nil {
				return repo.DirectorScene{}, directorProjectErr(err)
			}
		}
		return repo.DirectorScene{}, directorConflictError(saved, actor)
	}
	if err != nil {
		return repo.DirectorScene{}, wrapDirectorRepo(err)
	}
	s.publishDirectorScene(ctx, saved)
	return saved, nil
}

// prepareDirectorScene 校验正文、提取媒体引用并校验归属、算渲染指纹。
//
// 先迁移后校验：将来出现 v2 时，旧版本的正文要先升级再按当前规则校验；比服务端新的正文
// 由迁移给出「请等服务端升级」，而不是校验器的「不支持的结构版本」。
func (s *Service) prepareDirectorScene(ctx context.Context, ws int64, raw json.RawMessage) (preparedScene, error) {
	if len(raw) > director.MaxSceneBytes {
		return preparedScene{}, httpx.Errf(errcode.PayloadTooLarge, director.ErrSceneTooLarge.Error())
	}
	migrated, err := director.Migrate(raw)
	if err != nil {
		return preparedScene{}, directorSceneErr(err)
	}
	if err := director.Validate(migrated); err != nil {
		return preparedScene{}, directorSceneErr(err)
	}
	refs, err := director.MediaRefs(migrated)
	if err != nil {
		// Validate 已经核过媒体 ID 的形状，走到这里说明两边规则不一致 —— 按参数错误回报。
		return preparedScene{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := s.checkDirectorMedia(ctx, ws, refs); err != nil {
		return preparedScene{}, err
	}
	fp, err := director.RenderFingerprint(migrated)
	if err != nil {
		return preparedScene{}, httpx.Wrap(errcode.Internal, err)
	}
	return preparedScene{raw: migrated, fingerprint: fp, mediaIDs: director.MediaIDs(refs)}, nil
}

// purposeDirectorAsset 与 media/app 的导演台素材 purpose 是同一个值（DRD-15）。
// 刻意重写字面量而不是 import 媒体域（同 usageProjectCover 的理由）。
const purposeDirectorAsset = "director_asset"

// checkDirectorMedia 校验正文引用的媒体（全景、泼溅）属于本工作区，且类型对得上（DRD-16）。
//
// ⚠️ 不校验的话，把别人工作区的媒体 ID 写进自己的场景，就能挡住别人删除（删除保护按 media_ids 反查）。
// ⚠️ 不区分「不存在」与「不是你的」：区分开等于提供一个用 ID 探测别人有哪些文件的接口（同 CD-23）。
// ⚠️ 平台素材（workspace_id = 0）也拒：媒体域的按工作区查询会把它们一起查出来，
// 而用户的场景挡住的是平台的删除。
func (s *Service) checkDirectorMedia(ctx context.Context, ws int64, refs []director.MediaRef) error {
	if len(refs) == 0 {
		return nil
	}
	if s.mediaLookup == nil {
		// 查不到就放行等于把别人的资源当成已授权资产（同输入解析）。
		return httpx.Errf(errcode.Internal, "媒体查询未接入，无法校验场景引用的文件")
	}
	for _, ref := range refs {
		path := directorMediaPath(ref.Role)
		info, err := s.mediaLookup.LookupInWorkspace(ctx, ws, ref.ID)
		if err != nil || info.WorkspaceID != ws {
			return directorPathErr(path, "引用的文件不存在")
		}
		switch ref.Role {
		case director.MediaRolePanorama:
			if info.Kind != "image" {
				return directorPathErr(path, "全景背景必须是图片")
			}
		case director.MediaRoleSplat:
			// 泼溅的媒体大类（splat）由 DRT-03 接入；在那之前按 purpose 判：只有导演台的素材上传会写这个 purpose。
			if info.Purpose != purposeDirectorAsset {
				return directorPathErr(path, "泼溅背景必须是在导演台里上传的泼溅文件")
			}
		}
	}
	return nil
}

func directorMediaPath(role string) string {
	if role == director.MediaRoleSplat {
		return "splat.mediaId"
	}
	return "background.panorama.mediaId"
}

func directorPathErr(path, reason string) error {
	return httpx.Errf(errcode.InvalidArgument, "场景正文不合法："+path+"："+reason).
		WithDetails(map[string]any{"path": path})
}

// directorSceneErr 把 director/domain 的校验错误映射成对外错误：超限 41301，结构错误 40001 + details.path。
func directorSceneErr(err error) error {
	var ve *director.ValidationError
	switch {
	case errors.Is(err, director.ErrSceneTooLarge):
		return httpx.Errf(errcode.PayloadTooLarge, err.Error())
	case errors.Is(err, director.ErrSchemaTooNew):
		return httpx.Errf(errcode.InvalidArgument, err.Error()).
			WithDetails(map[string]any{"path": "schemaVersion"})
	case errors.As(err, &ve):
		return directorPathErr(ve.Path, ve.Reason)
	}
	return httpx.Wrap(errcode.Internal, err)
}

// directorProjectErr 改写 projectWritable 对回收站的文案（那句话写的是「文档」）。
func directorProjectErr(err error) error {
	var be *httpx.BizError
	if errors.As(err, &be) && be.Code == errcode.InvalidStateTransition {
		return httpx.Errf(errcode.InvalidStateTransition, "回收站中的项目不能修改导演台场景，请先恢复")
	}
	return err
}

func isNotFound(err error) bool {
	var be *httpx.BizError
	return errors.As(err, &be) && be.Code == errcode.NotFound
}

func wrapDirectorRepo(err error) error {
	if errors.Is(err, repo.ErrNotFound) {
		return httpx.Errf(errcode.NotFound, "场景不存在")
	}
	return wrapRepo(err)
}

func (s *Service) directorView(ctx context.Context, sc repo.DirectorScene) (DirectorSceneView, error) {
	records, err := s.repo.ListDirectorMedia(ctx, sc.WorkspaceID, sc.ProjectID, sc.ID)
	if err != nil {
		return DirectorSceneView{}, httpx.Wrap(errcode.Internal, err)
	}
	return DirectorSceneView{Scene: sc, Records: records}, nil
}

// directorBySelf 最后一次写入是不是当前用户**本人亲手**保存的（同文档与画布）。
//
// ⚠️ Agent / MCP 的写入不算：它们也带着用户本人的 ID，只比 EditorID 的话会把「Agent 刚摆的」
// 说成「你在另一个窗口改的」，用户点「以我为准」就覆盖掉它。机器发起的保存也谈不上「本人另一个窗口」。
func directorBySelf(cur repo.DirectorScene, actor DirectorActor) bool {
	return cur.Source == repo.EditorUser && actor.source() == repo.EditorUser &&
		cur.EditorID != 0 && cur.EditorID == actor.UserID
}

// directorConflictDetails 是冲突详情（VersionConflictError.details）。
//
// ⚠️ `currentSource` 与 `editorKind` 同值、两个都给（DRD-46）：前端复用文档节点的冲突文案函数，
// 它读 `currentSource`；而 `editorKind` 是画布冲突的既有键名，Agent / MCP 的重放逻辑认它。
func directorConflictDetails(cur repo.DirectorScene, actor DirectorActor) map[string]any {
	return map[string]any{
		"currentVersion":    cur.Version,
		"currentUpdatedAt":  cur.UpdatedAt.UTC().Format(time.RFC3339),
		"currentEditorId":   idOrEmpty(cur.EditorID),
		"currentEditorName": cur.EditorName,
		"currentSource":     cur.Source,
		"editorKind":        cur.Source,
		"agentSessionId":    idOrEmpty(cur.AgentSessionID),
		"mcpGrantId":        idOrEmpty(cur.McpGrantID),
		"bySelf":            directorBySelf(cur, actor),
	}
}

func directorConflictError(cur repo.DirectorScene, actor DirectorActor) error {
	return httpx.Errf(errcode.VersionConflict, directorConflictMessage(cur, directorBySelf(cur, actor))).
		WithDetails(directorConflictDetails(cur, actor))
}

func directorConflictMessage(cur repo.DirectorScene, bySelf bool) string {
	switch {
	case bySelf:
		return "你在另一个窗口修改过这个场景，请载入最新或选择以当前内容为准"
	case cur.Source == repo.EditorAgent:
		return "Agent 刚修改了这个场景，请载入最新后再保存"
	case cur.Source == repo.EditorMCP && cur.EditorName != "":
		return "外部 AI 客户端（代 " + cur.EditorName + "）刚修改了这个场景，请载入最新后再保存"
	case cur.Source == repo.EditorMCP:
		return "外部 AI 客户端刚修改了这个场景，请载入最新后再保存"
	case cur.EditorName != "":
		return cur.EditorName + " 修改了这个场景，请载入最新后再保存"
	default:
		return "场景已被修改，请载入最新后再保存"
	}
}
