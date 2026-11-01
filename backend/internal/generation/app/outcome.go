package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MediaArchiver 由媒体域实现：把产物字节写进对象存储并登记 media_object。
//
// ⚠️ 归档在**事务外**（远程调用），落库在事务内 —— 顺序是
// 「先归档拿到 mediaID，再一个事务写产物行 + 标成功」。
type MediaArchiver interface {
	Archive(ctx context.Context, in ArchiveInput) (mediaID int64, err error)
	// RegisterUsage 把产物登记为该任务的活动引用，使它不会被回收队列收走。
	RegisterUsage(ctx context.Context, mediaID int64, usageType string, refID int64) error
}

type ArchiveInput struct {
	WorkspaceID int64
	UserID      int64
	MIME        string
	Size        int64
	Body        io.Reader
}

// ProviderOutput 是适配器交回的一条产物。
type ProviderOutput struct {
	Type string // image / video / audio / text
	// Text 仅文本产物使用。
	Text string
	// Body 非文本产物的字节流；由归档器写进对象存储。
	Body io.Reader
	MIME string
	Size int64
	Meta json.RawMessage
}

func (e *Executor) SetArchiver(a MediaArchiver)  { e.archiver = a }
func (e *Executor) SetNotifier(n Notifier)       { e.notifier = n }
func (e *Executor) SetAssetBinder(b AssetBinder) { e.assetBinder = b }

// SetGroupBinder 接上组子项回写（P07-04）。
func (e *Executor) SetGroupBinder(b GroupItemBinder) { e.groupBinder = b }

// SetRunAdvancer 接上整图运行推进（P08-02）。
func (e *Executor) SetRunAdvancer(a RunAdvancer) { e.runAdvancer = a }
func (e *Executor) SetEvents(p EventPublisher)   { e.events = p }

// SetWorkRegistrar 接上作品自动登记。
func (e *Executor) SetWorkRegistrar(w WorkRegistrar) { e.works = w }

// WorkRegistrar 把一次成功的媒体生成登记成一件待审作品。
//
// 起因：用户不再手动提交作品，每次成功的图片/视频/音频生成自动进作品库
// （落 pending，公开仍要运营审核 + 加精）。端口定义在生成域这边，
// 由生成域自己的仓储实现（同成片的 `InsertWork`）—— Worker 不依赖内容域是有意的。
//
// ⚠️ 与通知同一套约束：**绝不能牵连主流程**。登记失败只告警 ——
// 产物已经归档、钱已经结了，为一条作品记录把任务判失败会把用户的产出一起退掉。
type WorkRegistrar interface {
	RegisterWork(ctx context.Context, id, generationID int64) (bool, error)
}

// AssetBinder 由脚本域实现：把出好的资产图回写到 script_asset。
//
// ⚠️ 不接的话资产会**永远停在 generating**：图其实已经出好并归档了，
// 但资产列表里那一项一直转圈，用户既看不到图也不敢重试。
//
// ⚠️ 与通知同一套约束：**绝不能牵连主流程**。回写失败只告警 ——
// 为一次状态同步把一个已完成的生成任务重跑一遍不值，
// 而且重跑会再扣一次费。
type AssetBinder interface {
	BindAsset(ctx context.Context, assetID, mediaID int64, status string) error
}

// GroupItemBinder 由脚本域实现：把子项的终态回写到 group_item，
// 并重算组的聚合状态。
//
// ⚠️ 与 AssetBinder 是同一类东西、同一套约束：不接的话子项**永远停在
// queued**（产物已经归档，组里那一格一直转圈）；回写失败**只告警**，
// 绝不牵连主流程 —— 为一次状态同步把已完成的任务重跑一遍会再扣一次费。
type GroupItemBinder interface {
	BindGroupItem(ctx context.Context, generationID int64, status, errMsg string) error
}

// RunAdvancer 由整图域实现：任务到终态后推进这次运行（NEW-04）。
//
// ⚠️ 与 GroupItemBinder 同一套约束：它跑在 Worker 里、跨域被调用，
// **绝不能牵连主流程** —— 出错只告警。不接的话整次运行永远停在
// running：节点跑完了，但没有人去解锁它的下游。
type RunAdvancer interface {
	AdvanceByGeneration(ctx context.Context, generationID int64,
		status, errCode, errMsg string) error
}

// SmartcutBinder 由智能剪辑实现：识别、片段级生成到终态时回写（docs/agent/smartcut SCD-32、SCD-33）。
//
// 识别成功把结果写进源素材的分析缓存，失败把那一行记失败（退款照常由生成链路按原批次完成）；
// 片段级生成成功把产物放回时间线（SCT-37）。
//
// ⚠️ 与 GroupItemBinder 同一套约束：**四条终态路径都要调**（成功、执行失败与超时、用户取消、运维补偿），
// 漏一条的话那一份素材的识别永远显示「识别中」，而且同一参数的分析行占着唯一索引，再点识别也建不出新任务。
// 回写失败只告警，绝不牵连主流程。
type SmartcutBinder interface {
	BindSmartcut(ctx context.Context, g repo.Generation, status string, outputs []repo.Output, errMsg string) error
}

// SetSmartcutBinder 接上智能剪辑的回写（Worker 的成功 / 失败路径）。
func (e *Executor) SetSmartcutBinder(b SmartcutBinder) { e.smartcut = b }

// SetSmartcutBinder 接上智能剪辑的回写 —— **取消与运维补偿路径也要回写**（理由同 SetGroupBinder）。
func (s *Service) SetSmartcutBinder(b SmartcutBinder) { s.smartcut = b }

// cancelMessage 是取消的原因文案：用户没写原因时记「用户取消」。
func cancelMessage(reason string) string {
	if reason == "" {
		return "用户取消"
	}
	return reason
}

// originOf 取快照里冻结的任务来源（空 = 常规来源）。
func originOf(g repo.Generation) string { return snapshotString(g.ParamsSnapshot, "origin") }

// bindSmartcut 只在任务带着智能剪辑的回写目标时调；失败只告警。
func bindSmartcut(ctx context.Context, b SmartcutBinder, log *slog.Logger, g repo.Generation, status string,
	outputs []repo.Output, errMsg string) {
	if b == nil || len(g.SmartcutTarget) == 0 {
		return
	}
	if err := b.BindSmartcut(ctx, g, status, outputs, errMsg); err != nil && log != nil {
		log.Warn("回写智能剪辑失败", "generationId", g.ID, "status", status, "error", err)
	}
}

// Notifier 发站内通知。
type Notifier interface {
	Notify(ctx context.Context, userID int64, typ, title, content, refType, refID string) error
}

// EventPublisher 推状态事件（WebSocket）。
//
// ⚠️ **事件不是事实来源**（GO-26）：前端收到事件后仍要用 HTTP 对账。
// 事件会乱序、会丢、会在断线期间错过，把它当唯一来源就会出现
// "界面显示还在生成，其实早就好了"。
type EventPublisher interface {
	PublishState(ctx context.Context, workspaceID int64, payload json.RawMessage) error
}

// Complete 归档产物并标成功。
//
// ⚠️ 顺序是**先归档、再落库标成功**（GO-23）：反过来的话存储失败时
// 用户已经看到"成功"却取不到产物 —— 那种失败既不触发退款也不会被重试捞起。
func (e *Executor) Complete(ctx context.Context, g repo.Generation,
	outputs []ProviderOutput, raw json.RawMessage) (bool, error) {
	// ⚠️ 适配器可能把产物落成了临时文件，Close 会连带删掉它。
	// 不管归档成功与否都要收干净 —— 漏一次就在盘上留一个谁也不认识的文件，
	// 而每跑一次任务多一个。
	defer closeOutputs(outputs)

	rows := make([]repo.Output, 0, len(outputs))
	archived := make([]int64, 0, len(outputs))

	for i, o := range outputs {
		if o.Type == "text" {
			rows = append(rows, repo.Output{Idx: i, OutputType: "text",
				TextContent: o.Text, Meta: o.Meta})
			continue
		}
		if e.archiver == nil {
			return false, fmt.Errorf("媒体归档器未接入，无法保存 %s 产物", o.Type)
		}
		mediaID, err := e.archiver.Archive(ctx, ArchiveInput{
			WorkspaceID: g.WorkspaceID, UserID: g.CreatedByUserID,
			MIME: o.MIME, Size: o.Size, Body: o.Body,
		})
		if err != nil {
			// ⚠️ 归档失败**不标成功**：让任务留在 running，由重试或恢复扫描处理。
			// 已经写进对象存储的那几个会成为孤儿，由媒体回收队列按
			// 「无引用 + 超保留期」收走（CD-23）。
			return false, fmt.Errorf("归档第 %d 个产物失败: %w", i, err)
		}
		archived = append(archived, mediaID)
		rows = append(rows, repo.Output{Idx: i, OutputType: o.Type,
			MediaObjectID: mediaID, Meta: o.Meta})
	}

	ok, err := e.repo.SaveOutputsAndFinish(ctx, g.ID, e.nextID, rows, raw)
	if err != nil {
		return false, err
	}
	if !ok {
		// 任务已被取消或已判超时：产物不算数，刚归档的媒体交给回收队列。
		e.log.Info("产物归档后发现任务已是终态，跳过", "generationId", g.ID)
		return false, nil
	}

	// 登记引用：产物属于这个任务，回收队列不该收走它。
	for _, mediaID := range archived {
		if err := e.archiver.RegisterUsage(ctx, mediaID, "generation_output", g.ID); err != nil {
			// 登记失败不回滚成功状态：产物已经在库里了，最坏是它被回收队列
			// 在保留期后收走，比让一个已完成的任务变回失败轻得多。
			e.log.Error("登记产物引用失败，可能被回收队列收走",
				"generationId", g.ID, "mediaId", mediaID, "error", err)
		}
	}

	e.bindAsset(ctx, g, firstMediaID(archived), domain.StatusSuccess)
	e.registerWork(ctx, g, firstMediaID(archived))
	e.bindGroupItem(ctx, g, domain.StatusSuccess, "")
	e.advanceRun(ctx, g, domain.StatusSuccess, "", "")
	bindSmartcut(ctx, e.smartcut, e.log, g, domain.StatusSuccess, rows, "")
	e.afterTerminal(ctx, g, domain.StatusSuccess, rows, "", "")
	return true, nil
}

// registerWork 成功的媒体生成自动登记为待审作品。没有媒体产出（纯文本）的不登记。
func (e *Executor) registerWork(ctx context.Context, g repo.Generation, mediaID int64) {
	if e.works == nil || mediaID == 0 {
		return
	}
	if _, err := e.works.RegisterWork(ctx, e.nextID(), g.ID); err != nil {
		e.log.Warn("生成登记作品失败", "generationId", g.ID, "error", err)
	}
}

// bindAsset 把资产图回写到脚本域。
func (e *Executor) bindAsset(ctx context.Context, g repo.Generation,
	mediaID int64, status string) {
	if e.assetBinder == nil || g.ScriptAssetID == 0 {
		return
	}
	assetStatus := "ready"
	if status != domain.StatusSuccess {
		assetStatus = "failed"
	}
	if err := e.assetBinder.BindAsset(ctx, g.ScriptAssetID, mediaID, assetStatus); err != nil {
		e.log.Warn("回写资产状态失败，资产可能停在 generating",
			"generationId", g.ID, "scriptAssetId", g.ScriptAssetID, "error", err)
	}
}

// bindGroupItem 把子项终态回写到脚本域。
func (e *Executor) bindGroupItem(ctx context.Context, g repo.Generation,
	status, errMsg string) {
	if e.groupBinder == nil || g.GroupItemID == 0 {
		return
	}
	if err := e.groupBinder.BindGroupItem(ctx, g.ID, status, errMsg); err != nil {
		e.log.Warn("回写组子项失败，子项可能停在 queued",
			"generationId", g.ID, "groupItemId", g.GroupItemID, "error", err)
	}
}

// advanceRun 推进整图运行。
func (e *Executor) advanceRun(ctx context.Context, g repo.Generation,
	status, errCode, errMsg string) {
	if e.runAdvancer == nil {
		return
	}
	if err := e.runAdvancer.AdvanceByGeneration(ctx, g.ID, status, errCode, errMsg); err != nil {
		e.log.Warn("推进整图运行失败，运行可能停在 running",
			"generationId", g.ID, "error", err)
	}
}

func firstMediaID(archived []int64) int64 {
	if len(archived) == 0 {
		return 0
	}
	return archived[0]
}

func closeOutputs(outputs []ProviderOutput) {
	for _, o := range outputs {
		if c, ok := o.Body.(io.Closer); ok {
			_ = c.Close()
		}
	}
}

// afterTerminal 终态后的通知与事件。outputs 只在成功时有，code / errMsg 只在失败时有。
//
// ⚠️ 只在**真的改动了状态**之后调用（Finish 返回 true）：
// 晚到的结果不该再发一次通知，用户会收到两条自相矛盾的消息。
func (e *Executor) afterTerminal(ctx context.Context, g repo.Generation, status string,
	outputs []repo.Output, code, errMsg string) {
	publishUpdate(ctx, e.events, e.log, g, status, outputs, code, errMsg)
	// 智能剪辑名下的任务不发站内通知（剪辑台自己显示进度），事件照发。
	if e.notifier == nil || g.CreatedByUserID == 0 || !domain.Notifies(originOf(g)) {
		return
	}
	title, typ := "生成完成", "generation_success"
	content := ""
	if status != domain.StatusSuccess {
		title, typ = "生成失败", "generation_failed"
		content = errMsg
	}
	if err := e.notifier.Notify(ctx, g.CreatedByUserID, typ, title, content,
		"generation", itoa(g.ID)); err != nil {
		// 通知绝不能牵连主流程：任务已经落定，为一条通知重跑一次生成不值。
		e.log.Warn("发送通知失败", "generationId", g.ID, "error", err)
	}
}

// Cancel 用户取消任务。
//
// ⚠️ 取消是**竞争**：用户点取消的同时 Worker 可能正要写成功。
// 谁先拿到状态条件谁赢，另一方的写入影响 0 行 —— 这正是 CAS 要解决的（FLOW-04）。
// 已经跑完的任务取消不了：那时钱已经花在上游了，假装取消成功
// 只会让用户以为不用付费。
func (s *Service) Cancel(ctx context.Context, workspaceID, id int64, reason string) error {
	g, err := s.repo.Get(ctx, workspaceID, id)
	if err != nil {
		return httpx.Err(errcode.NotFound)
	}
	ok, err := s.repo.Cancel(ctx, workspaceID, id, reason)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if !ok {
		return httpx.Errf(errcode.InvalidStateTransition,
			"任务已经"+statusLabel(g.Status)+"，无法取消")
	}
	if s.groupBinder != nil && g.GroupItemID != 0 {
		if err := s.groupBinder.BindGroupItem(ctx, id, domain.StatusCancelled, reason); err != nil {
			s.logWarn(ctx, "取消后回写组子项失败，子项可能停在 queued", id, err)
		}
	}
	if s.runAdvancer != nil {
		if err := s.runAdvancer.AdvanceByGeneration(ctx, id,
			domain.StatusCancelled, "cancelled", reason); err != nil {
			s.logWarn(ctx, "取消后推进整图运行失败，运行可能停在 running", id, err)
		}
	}
	bindSmartcut(ctx, s.smartcut, s.log, g, domain.StatusCancelled, nil, cancelMessage(reason))
	if s.refunder != nil && g.CreditsCost > 0 {
		if err := s.refunder.Refund(ctx, g.PayerAccountID, "generation", itoa(id),
			"用户取消"); err != nil {
			// 退款失败只告警：取消已经生效，退款的一次性由账本唯一索引兜底，
			// 失败的由对账作业补。
			s.logWarn(ctx, "取消退款失败，需对账补偿", id, err)
		}
	}
	publishUpdate(ctx, s.events, s.log, g, domain.StatusCancelled, nil, domain.ErrorCodeCancelled, cancelMessage(reason))
	return nil
}

func (s *Service) SetRefunder(r Refunder) { s.refunder = r }

// SetGroupBinder 接上组子项回写 —— **取消路径也要回写**。
//
// ⚠️ 取消走的是 Service 不是 Executor：只在 Executor 那侧接的话，
// 用户从任务列表取消一个组子项，generation 变成 cancelled
// 而组里那一格还显示"排队中"，组的聚合状态因此永远停在 running，
// 整组再也执行不了（CanExecuteGroup 会拦住）。
func (s *Service) SetGroupBinder(b GroupItemBinder) { s.groupBinder = b }

// SetRunAdvancer 接上整图运行推进 —— **取消路径也要推进**。
//
// ⚠️ 理由同 SetGroupBinder：只在 Worker 那侧接的话，用户从任务列表
// 取消一个整图节点，generation 变成 cancelled 而运行还停在 running，
// 既不结束也不推进下游。
func (s *Service) SetRunAdvancer(a RunAdvancer) { s.runAdvancer = a }

func statusLabel(s string) string {
	switch s {
	case domain.StatusSuccess:
		return "完成"
	case domain.StatusFailed:
		return "失败"
	case domain.StatusCancelled:
		return "取消"
	}
	return s
}

// Outputs 取产物列表。
func (s *Service) Outputs(ctx context.Context, workspaceID, id int64) ([]repo.Output, error) {
	if _, err := s.repo.Get(ctx, workspaceID, id); err != nil {
		return nil, httpx.Err(errcode.NotFound)
	}
	out, err := s.repo.ListOutputs(ctx, id)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return out, nil
}

// Output 取指定序号的产物（受保护读取用）。
func (s *Service) Output(ctx context.Context, workspaceID, id int64, idx int) (repo.Output, error) {
	o, err := s.repo.GetOutput(ctx, workspaceID, id, idx)
	if err != nil {
		return repo.Output{}, httpx.Err(errcode.NotFound)
	}
	return o, nil
}
