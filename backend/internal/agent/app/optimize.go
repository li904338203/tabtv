package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 一键优化提示词（AGT-43，设计 02 §4.1）：在 API 进程里同步调一次推理模型，把输入框里的正文改写得
// 更具体，结果交给前端以「替换 / 保留原文」让用户确认 —— **这里不改任何用户数据**。
//
// 计费外壳照抄轮次运行器（runner.charge / finish）：报价（operation=agent_chat）→ 建调用记录 →
// 扣费（refType=agent_turn，refID=调用记录 id）→ 调模型 → **先校验非空** → 通过才结算；
// 任一步失败按「本次是否真的结束了这条记录」退款。
//
// ⚠️ 结算晚于校验（§5.7）：模型返回了空串 / 只有空白 / 超长时整次判失败并全额退款 ——
// 让用户为一段空文字付费更糟。
// ⚠️ 每次点击新建一条记录（refID 不复用）：用同一个引用的话第二次优化就撞账本唯一约束；
// 失败也保留记录（§5.7 轮次坑：只按产出计轮的话重试会命中上一次的 failed）。
// ⚠️ 幂等键只防「同一次点击的网络重试」：同键重放成功的那一次原样返回、不再扣费。

const (
	// optimizeCallTimeout 单次优化的模型调用上限。改写一段几百字的需求不该超过这个数；
	// 路由层（cmd/api longRunningRoutes）给的上限更长，由这里先触发、带上准确的错误。
	optimizeCallTimeout = 3 * time.Minute
	// OptimizeStaleAfter 恢复扫描的判死阈值：大于路由层的同步模型请求上限（15 分钟），
	// 扫到的一定已经没有请求在等它了（否则会把一个还在等模型的调用判失败并退款，而它随后成功）。
	OptimizeStaleAfter = 20 * time.Minute
	// optimizeReason 账本流水上的说明。
	optimizeReason = "一键优化提示词"
	// optimizeStaleReason 恢复扫描判失败时的原因（也写进退款流水）。
	optimizeStaleReason = "优化超时未结束（请求被中断或服务重启），已退款"
)

// optimizeSystemPrompt 是改写指令。只写「怎么改、输出什么形状」—— 输出格式是代码与前端之间的契约
// （前端把它整段填回输入框），不交给可运营调优的模板。
const optimizeSystemPrompt = `你是短视频创作的提示词优化助手。用户会给你一段他在创作输入框里写的需求，请把它改写得更清晰、具体、可执行，方便后续由 AI 规划并生成视频。

改写要求：
1. 保留用户的原意与全部明确要求（题材、人物、视角、时长、画幅、风格等），不得改变或删减；
2. 只补充有助于创作的具体细节：主体与场景、情绪基调、镜头语言、节奏、画面风格；不要编造与原意冲突的设定；
3. 用户没有提到的硬性参数（时长、画幅、分辨率）不要擅自指定；
4. 使用与用户相同的语言，写成一段连贯的自然语言描述，控制在 300 字以内；
5. 只输出改写后的正文：不要解释、不要标题、不要列表符号、不要加引号或代码块。`

// characterAppearancePrompt 是「AI 帮写」角色形象描述的改写指令（MKT-T-35、dev/05 MKD-29）。
//
// 与 optimizeSystemPrompt 同一条链（报价、表、账本流水、退款、恢复扫描全部不变），只换指令：
// 用通用指令的话模型会把一段外观描述改写成「短视频创作需求」（分镜、镜头语言），能用但答非所问。
// ⚠️ 只写外观、不写动作剧情；不出现真实人物姓名；措辞不承诺「真人 / 复刻某人」（MK-23、MK-25）。
// ⚠️ 输出形状与通用指令一致（纯文本、不加解释）：前端照样整段填回描述框，CleanOptimizedText 照样清理。
const characterAppearancePrompt = `你是营销视频虚拟角色的形象描述助手。用户会给你一段他为一个非真人角色（卡通形象、拟人形象、吉祥物、产品拟人等）写的形象描述，请把它改写得更清晰、具体，方便后续由 AI 生成这个角色的设定图。

改写要求：
1. 保留用户的原意与全部明确设定（物种或原型、性别气质、年龄感、配色、风格等），不得改变或删减；
2. 只描述角色的外观：体型与比例、材质与质感、配色、五官与常态表情、发型或头部特征、服饰与配件；
3. 不写动作、情节、场景、镜头与台词；不写任何真实人物的姓名，也不要把角色写成某位真实人物的样子；
4. 用户没有提到的设定可以补充，但要与原意一致、不要相互矛盾；
5. 使用与用户相同的语言，写成一段连贯的自然语言描述，控制在 300 字以内；
6. 只输出改写后的正文：不要解释、不要标题、不要列表符号、不要加引号或代码块。`

// 优化用途（POST /agent/prompt-optimize 的可选 `purpose`）。缺省 = OptimizePurposeGeneral，行为与改动前逐字相同。
const (
	OptimizePurposeGeneral             = "general"
	OptimizePurposeCharacterAppearance = "character_appearance"
)

// optimizeSystemFor 按用途选改写指令；未知用途返回 false（调用方回 40001）。
func optimizeSystemFor(purpose string) (string, bool) {
	switch purpose {
	case "", OptimizePurposeGeneral:
		return optimizeSystemPrompt, true
	case OptimizePurposeCharacterAppearance:
		return characterAppearancePrompt, true
	}
	return "", false
}

// OptimizeInput 是一次优化请求。
type OptimizeInput struct {
	IdempotencyKey string
	Text           string
	// ModelCode 所选推理模型；空 = 平台第一个可用的推理模型。
	ModelCode string
	// Purpose 改写用途（OptimizePurpose*）；空 = general。只换改写指令，计费链路不变。
	Purpose string
}

// OptimizeResult 是优化结果。
type OptimizeResult struct {
	ID        int64
	Text      string
	ModelCode string
	Credits   int
	// Replayed 同一个幂等键已经成功过，返回的是那一次的结果（没有再扣费）。
	Replayed bool
}

// OptimizePorts 是一键优化要的端口（与 Worker 侧运行器同一组适配器，见 wiring.AgentChatModel 等）。
type OptimizePorts struct {
	Chat      ChatModel
	Quoter    Quoter
	Ledger    Ledger
	Reasoning ReasoningDefaults
}

// OptimizePrompt 同步改写一段提示词（见文件头）。
func (s *Service) OptimizePrompt(ctx context.Context, a SessionActor, in OptimizeInput) (OptimizeResult, error) {
	op := s.p.Optimize
	if op == nil || op.Chat == nil || op.Quoter == nil || op.Ledger == nil {
		return OptimizeResult{}, httpx.Errf(errcode.Internal, "推理模型链路未接入")
	}
	if err := validateIdemKey(in.IdempotencyKey); err != nil {
		return OptimizeResult{}, err
	}
	// 用途在一切副作用之前判：未知值不能落成「按通用指令改写并扣费」。
	system, ok := optimizeSystemFor(in.Purpose)
	if !ok {
		return OptimizeResult{}, httpx.Errf(errcode.InvalidArgument,
			"purpose 只能是 general 或 character_appearance").
			WithDetails(map[string]any{"field": "purpose", "code": "bad_value"})
	}
	if replay, done, err := s.replayOptimize(ctx, a, in.IdempotencyKey); done || err != nil {
		return replay, err
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return OptimizeResult{}, httpx.Errf(errcode.InvalidArgument, "先写下想优化的内容")
	}
	if utf8.RuneCountInString(text) > MaxMessageRunes {
		return OptimizeResult{}, httpx.Errf(errcode.InvalidArgument, "内容不能超过 "+strconv.Itoa(MaxMessageRunes)+" 字")
	}

	// ① 推理模型：所选的；没选用平台第一个可用的（与运行器同一个兜底）。
	model := strings.TrimSpace(in.ModelCode)
	if model == "" && op.Reasoning != nil {
		code, err := op.Reasoning.DefaultReasoningModel(ctx, a.MemberLevelID)
		if err != nil {
			return OptimizeResult{}, httpx.Wrap(errcode.Internal, err)
		}
		model = code
	}
	if model == "" {
		return OptimizeResult{}, httpx.Errf(errcode.ModelUnavailable, "还没有可用的推理模型")
	}

	// ② 报价（agent_chat，按轮）。报价错误原样返回（不可用的原因在里面，如需要会员）。
	q, err := op.Quoter.QuoteTurn(ctx, model, a.MemberLevelID)
	if err != nil {
		return OptimizeResult{}, err
	}

	// ③ 先建调用记录再扣费：扣费成功而进程崩在下一步时，恢复扫描从这一行知道扣了多少。
	rec, err := s.repo.InsertOptimize(ctx, repo.PromptOptimize{
		ID: s.nextID(), WorkspaceID: a.WorkspaceID, PayerAccountID: a.PayerAccountID, UserID: a.UserID,
		ModelCode: model, IdempotencyKey: in.IdempotencyKey, CreditsCost: q.Credits, InputText: text,
	})
	if errors.Is(err, repo.ErrDuplicate) {
		// 并发的同键请求：另一个已经建了记录，按它的结局回答。
		if replay, done, e2 := s.replayOptimize(ctx, a, in.IdempotencyKey); done || e2 != nil {
			return replay, e2
		}
		return OptimizeResult{}, httpx.Errf(errcode.Conflict, "并发请求冲突，请重试")
	}
	if err != nil {
		return OptimizeResult{}, httpx.Wrap(errcode.Internal, err)
	}
	refID := idStr(rec.ID)

	// ④ 扣费。余额不足 / 账本出错：这次什么都没扣，记失败不退款。
	if q.Credits > 0 {
		err := op.Ledger.Consume(ctx, a.PayerAccountID, a.UserID, q.Credits, RefTypeTurn, refID, optimizeReason)
		switch {
		case err == nil, errors.Is(err, ErrLedgerDuplicate):
		case errors.Is(err, ErrInsufficientCredits):
			s.failOptimize(ctx, rec, 0, ErrCodeInsufficientCredits, err.Error())
			bal := -1
			if s.p.Accounts != nil {
				if b, e := s.p.Accounts.Balance(ctx, a.PayerAccountID); e == nil {
					bal = b
				}
			}
			if bal >= 0 && bal < q.Credits {
				return OptimizeResult{}, insufficient(q.Credits, bal)
			}
			return OptimizeResult{}, httpx.Errf(errcode.InsufficientCredits, "积分不足：优化一次需要 "+strconv.Itoa(q.Credits))
		default:
			s.failOptimize(ctx, rec, 0, ErrCodeLedger, err.Error())
			return OptimizeResult{}, httpx.Wrap(errcode.Internal, fmt.Errorf("扣费失败: %w", err))
		}
	}

	// ⑤ 调模型（不带工具、不流式回推）。
	callCtx, cancel := context.WithTimeout(ctx, optimizeCallTimeout)
	out, callErr := op.Chat.Chat(callCtx, ChatInput{
		ModelCode: model, System: system,
		Messages: []ChatMessage{{Role: "user", Content: text}},
	}, func(string) {})
	cancel()

	// 从这里起结账用脱离请求取消的上下文：模型已经返回了，要么把结果记下，要么把钱退回去。
	sctx, scancel := settleContext(ctx)
	defer scancel()

	// ⑥ 先校验，通过才结算。
	if callErr != nil {
		s.failOptimize(sctx, rec, q.Credits, ErrCodeModel, callErr.Error())
		return OptimizeResult{}, httpx.Errf(errcode.ProviderError, "优化没有完成："+callErr.Error()+refundNote(q.Credits))
	}
	result, reason := CleanOptimizedText(out.Text)
	if reason != "" {
		s.failOptimize(sctx, rec, q.Credits, ErrCodeEmptyReply, reason)
		return OptimizeResult{}, httpx.Errf(errcode.ProviderError, reason+refundNote(q.Credits))
	}
	changed, err := s.repo.FinishOptimize(sctx, rec.ID, repo.OptimizeSuccess, result, "", "")
	if err != nil {
		// 结果已经拿到、钱也扣了：落库失败不退款也不报错，用户拿到他付费买的改写。
		// 记录停在 running，恢复扫描会判它超时并退款 —— 那是对用户有利的一侧。
		s.log.Warn("优化结果落库失败", "optimizeId", rec.ID, "error", err)
	} else if !changed {
		// 恢复扫描抢先判了失败（并已退款）：以库里为准，不把已退款的结果再交出去。
		return OptimizeResult{}, httpx.Errf(errcode.ProviderError, "这次优化已超时作废并退款，请重新点击")
	}
	return OptimizeResult{ID: rec.ID, Text: result, ModelCode: model, Credits: q.Credits}, nil
}

// replayOptimize 同一个幂等键已有记录时按它的结局回答。done=false 表示没有记录（继续走正常流程）。
func (s *Service) replayOptimize(ctx context.Context, a SessionActor, key string) (OptimizeResult, bool, error) {
	rec, err := s.repo.GetOptimizeByKey(ctx, a.WorkspaceID, a.UserID, key)
	if errors.Is(err, repo.ErrNotFound) {
		return OptimizeResult{}, false, nil
	}
	if err != nil {
		return OptimizeResult{}, true, httpx.Wrap(errcode.Internal, err)
	}
	switch rec.Status {
	case repo.OptimizeSuccess:
		return OptimizeResult{ID: rec.ID, Text: rec.OutputText, ModelCode: rec.ModelCode,
			Credits: rec.CreditsCost, Replayed: true}, true, nil
	case repo.OptimizeRunning:
		return OptimizeResult{}, true, httpx.Errf(errcode.Conflict, "这次优化还在进行，请稍候")
	default:
		// ⚠️ 失败的那一次不再重跑：同一个键重跑就是同一个账本引用，会撞「已退款」。
		// 前端每次点击都换新键，走到这里只可能是同一次点击的重放。
		msg := rec.ErrorMessage
		if msg == "" {
			msg = "这次优化没有完成"
		}
		return OptimizeResult{}, true, httpx.Errf(errcode.Conflict, msg+"，请重新点击优化")
	}
}

// failOptimize 记失败；charged>0 且本次真的结束了这条记录时全额退款（按原批次，账本保证只退一次）。
func (s *Service) failOptimize(ctx context.Context, rec repo.PromptOptimize, charged int, code, msg string) {
	changed, err := s.repo.FinishOptimize(ctx, rec.ID, repo.OptimizeFailed, "", code, msg)
	if err != nil {
		s.log.Error("优化调用落失败终态出错（恢复扫描会兜底退款）", "optimizeId", rec.ID, "error", err)
		return
	}
	if !changed || charged <= 0 {
		return
	}
	if err := s.p.Optimize.Ledger.Refund(ctx, rec.PayerAccountID, RefTypeTurn, idStr(rec.ID), optimizeReason+"失败退款"); err != nil {
		s.log.Warn("优化失败退款出错，需对账补偿", "optimizeId", rec.ID, "payerAccountId", rec.PayerAccountID,
			"credits", charged, "error", err)
	}
}

func refundNote(credits int) string {
	if credits <= 0 {
		return ""
	}
	return "（已全额退回 ✦" + strconv.Itoa(credits) + "）"
}

// CleanOptimizedText 清理模型输出（去掉首尾空白、整段包裹的代码块与引号），判定是否可用。
// reason 非空表示这次产出不能用（空 / 超长），调用方据此全额退款。
func CleanOptimizedText(raw string) (text, reason string) {
	t := strings.TrimSpace(raw)
	if strings.HasPrefix(t, "```") && strings.HasSuffix(t, "```") && len(t) >= 6 {
		t = strings.TrimSpace(t[3 : len(t)-3])
		// 代码块首行可能是语言标记（```text）。
		if i := strings.IndexByte(t, '\n'); i >= 0 && i < 16 && !strings.ContainsAny(t[:i], " \t，。,.") {
			t = strings.TrimSpace(t[i+1:])
		}
	}
	for _, pair := range [][2]string{{"“", "”"}, {"\"", "\""}, {"「", "」"}, {"『", "』"}} {
		if strings.HasPrefix(t, pair[0]) && strings.HasSuffix(t, pair[1]) &&
			len(t) >= len(pair[0])+len(pair[1]) &&
			!strings.Contains(t[len(pair[0]):len(t)-len(pair[1])], pair[1]) {
			t = strings.TrimSpace(t[len(pair[0]) : len(t)-len(pair[1])])
		}
	}
	if t == "" {
		return "", "模型没有返回可用的改写结果"
	}
	if utf8.RuneCountInString(t) > MaxMessageRunes {
		return "", "改写结果超过 " + strconv.Itoa(MaxMessageRunes) + " 字，无法填回输入框"
	}
	return t, ""
}

// RecoverStaleOptimizes 把停在 running 太久的优化调用判失败并退款（API 进程的恢复扫描调用）。
//
// ⚠️ 顺序是**先退款、再判失败**（同 script/app.RecoverStaleCalls）：退款幂等，判失败是 CAS。
// 反过来的话判完失败而退款没成，下一轮就再也捞不到它了。多个 API 实例同时扫也安全。
func (s *Service) RecoverStaleOptimizes(ctx context.Context) (int, error) {
	if s.p.Optimize == nil || s.p.Optimize.Ledger == nil {
		return 0, nil
	}
	recs, err := s.repo.StaleRunningOptimizes(ctx, OptimizeStaleAfter, 100)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range recs {
		if r.CreditsCost > 0 {
			err := s.p.Optimize.Ledger.Refund(ctx, r.PayerAccountID, RefTypeTurn, idStr(r.ID), optimizeStaleReason)
			// 找不到原消费流水 = 当时没扣成（崩在扣费之前），没什么可退的。
			if err != nil && !isLedgerNotFound(err) {
				s.log.Warn("优化恢复扫描退款失败，下一轮再试", "optimizeId", r.ID, "error", err)
				continue
			}
		}
		changed, err := s.repo.FinishOptimize(ctx, r.ID, repo.OptimizeFailed, "", ErrCodeModel, optimizeStaleReason)
		if err != nil {
			s.log.Warn("优化恢复扫描写终态失败，下一轮再试", "optimizeId", r.ID, "error", err)
			continue
		}
		if changed {
			n++
		}
	}
	return n, nil
}

// ErrLedgerNotFound 退款时找不到原消费流水（由 wiring 适配器翻译）。
var ErrLedgerNotFound = errors.New("找不到原消费流水")

func isLedgerNotFound(err error) bool { return errors.Is(err, ErrLedgerNotFound) }
