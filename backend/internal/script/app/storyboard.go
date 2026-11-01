package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// StoryboardInput 是一次分镜生成请求。
type StoryboardInput struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ScriptID       int64
	ModelCode      string
	MemberLevelID  int64
	// ShotCount 期望镜头数；0 表示由模型自己定。
	ShotCount int
	// ExtraContext 手工镜头上下文或角色参考（AV-06 的"组装上下文"）。
	ExtraContext string
}

// StoryboardResult 是一次分镜生成的结果。
type StoryboardResult struct {
	Detail  Detail
	CallID  int64
	Credits int
}

// GenerateStoryboard 同步生成分镜。
//
// ⚠️ **顺序是这整个方法最重要的东西**（老系统最贵的一个 bug 就在这）：
//
//	① 校验输入 → ② 报价 → ③ 建调用记录（幂等键去重）→ ④ 扣费
//	→ ⑤ 调模型 → ⑥ **先解析** → ⑦ 解析通过才落库并标 success
//	→ 任一步失败：退款 + 标 failed
//
// 老系统的顺序是「调模型 → 结算 → 解析」，于是分镜 JSON 解析失败时
// 已经扣了 60 积分、库里落 0 个镜头 —— 用户看到一个空脚本和一笔
// 说不清的消费。这个方法**没有事务可回滚**（事务内禁止远程调用），
// 扣款一旦发生就只能靠退款补，所以顺序就是唯一的保护。
func (s *Service) GenerateStoryboard(ctx context.Context, in StoryboardInput) (
	StoryboardResult, error) {
	if s.model == nil || s.quoter == nil || s.ledger == nil {
		return StoryboardResult{}, httpx.Errf(errcode.Internal, "文本模型链路未接入")
	}
	sc, err := s.repo.Get(ctx, in.WorkspaceID, in.ScriptID)
	if err != nil {
		return StoryboardResult{}, translate(err)
	}

	// ① 报价顺带拿到模型上限。⚠️ 上限来自模型配置，不写死在 DTO 上。
	quote, err := s.quoter.QuoteText(ctx, TextQuoteAsk{
		ModelCode: in.ModelCode, OperationCode: string(domain.PurposeStoryboard),
		Text: sc.RawInput + in.ExtraContext, MemberLevelID: in.MemberLevelID,
	})
	if err != nil {
		return StoryboardResult{}, err
	}
	if err := domain.ValidateRawInput(sc.RawInput, quote.MaxInputChars); err != nil {
		return StoryboardResult{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}

	// ② 轮次与幂等键。⚠️ 轮次按调用次数算（含失败），否则重试会命中
	// 上一次的 failed 记录，用户再也点不动「重新生成」。
	round, err := s.repo.NextRound(ctx, sc.ID, domain.PurposeStoryboard)
	if err != nil {
		return StoryboardResult{}, httpx.Wrap(errcode.Internal, err)
	}
	key := domain.IdempotencyKey(domain.PurposeStoryboard, sc.ID, round, in.ModelCode)

	callID := s.nextID()
	if err := s.repo.InsertCall(ctx, repo.LLMCall{
		ID: callID, WorkspaceID: sc.WorkspaceID, PayerAccountID: in.PayerAccountID,
		ScriptID: sc.ID, Purpose: string(domain.PurposeStoryboard),
		ModelID: quote.ModelID, IdempotencyKey: key, CreditsCost: quote.Credits,
		// 谁发起的（NEW-20）。账本那侧也记（operator_user_id），
		// 但排查"这 60 积分谁花的"时多一次反查就多一次出错机会。
		CreatedByUserID: in.UserID,
	}); err != nil {
		return StoryboardResult{}, translate(err)
	}

	// ③ 扣费。⚠️ ref 用 **callID** 而不是 scriptID：一个脚本会生成很多次，
	// 用 scriptID 做引用的话第二次就撞账本的 `(ref_id, type)` 唯一约束，
	// 表现为「重新生成一次就报重复扣费」。
	refID := strconv.FormatInt(callID, 10)
	if quote.Credits > 0 {
		if err := s.ledger.Consume(ctx, in.PayerAccountID, in.UserID, quote.Credits,
			"script_llm", refID, "分镜生成"); err != nil {
			_ = s.repo.FinishCall(ctx, callID, domain.LLMFailed, nil, "扣费失败")
			return StoryboardResult{}, err
		}
	}

	// ④ 调模型 + ⑤ 解析。两步的错误合并成一个结账决定。
	system, user, err := s.buildStoryboardPrompt(ctx, sc, in)
	if err != nil {
		s.abortCall(ctx, callID, in.PayerAccountID, refID, quote.Credits,
			domain.SettleDecision{Refund: true, Status: domain.LLMFailed, Reason: err.Error()})
		return StoryboardResult{}, httpx.Errf(errcode.Internal, err.Error())
	}
	raw, callErr := s.model.Complete(ctx, TextRequest{
		ModelCode: in.ModelCode, System: system, User: user,
	})
	// 从这里起结账、落库都用脱离请求取消的上下文：模型已经返回了，要么把付了钱的镜头写进去，要么把钱退回去。
	ctx, cancel := settleContext(ctx)
	defer cancel()
	var board domain.Storyboard
	var parseErr error
	if callErr == nil {
		board, parseErr = domain.ParseStoryboard(raw)
	}

	decision := domain.DecideSettle(callErr, parseErr)
	if !decision.Settle {
		s.abortCall(ctx, callID, in.PayerAccountID, refID, quote.Credits, decision)
		// ⚠️ 错误里要带上**模型说了什么**：只说"生成失败"的话，
		// 用户不知道是该改剧情、换模型，还是等一会儿重试。
		// 用 PROVIDER_ERROR 而不是 500：这不是我们内部坏了，
		// 而且它标了 retryable —— 前端据此可以给一个「重试」按钮。
		return StoryboardResult{}, httpx.Errf(errcode.ProviderError, decision.Reason)
	}

	// ⑥ 落库。到这里产出已经确认可用。
	if err := s.repo.ReplaceShots(ctx, sc.ID, s.nextID, board.Shots); err != nil {
		// ⚠️ 落库失败也要退款：用户付了钱却没拿到镜头。
		s.abortCall(ctx, callID, in.PayerAccountID, refID, quote.Credits,
			domain.SettleDecision{Refund: true, Status: domain.LLMFailed,
				Reason: "镜头写入失败：" + err.Error()})
		return StoryboardResult{}, httpx.Wrap(errcode.Internal, err)
	}
	profiles := rawJSON(board.Assets)
	style := board.GlobalStyle
	patch := repo.PatchInput{
		ID: sc.ID, WorkspaceID: sc.WorkspaceID, ExpectedVersion: sc.Version,
		AssetProfiles: profiles,
	}
	if style != "" {
		patch.GlobalStyle = &style
	}
	updated, err := s.repo.Patch(ctx, patch)
	if err != nil {
		// ⚠️ 镜头已经写进去了，档案没写上 —— **不退款也不报错**：
		// 用户拿到了他付费买的镜头表。档案缺失会在资产 reconcile 时
		// 退回模板描述（P07-02），比把整次生成判失败温和得多。
		s.warn(ctx, "分镜已落库但档案未更新", "scriptId", sc.ID, "error", err)
		updated = sc
	}
	_ = s.repo.FinishCall(ctx, callID, domain.LLMSuccess, rawJSON(board), "")

	detail, err := s.detail(ctx, updated)
	if err != nil {
		return StoryboardResult{}, err
	}
	return StoryboardResult{Detail: detail, CallID: callID, Credits: quote.Credits}, nil
}

// settleTimeout 是模型返回之后、结账与落库这一段的上限（只有几次写库）。
const settleTimeout = 30 * time.Second

// SyncLLMRequestTimeout 是「在 API 进程里同步调用模型」的请求（分镜生成、提示词 AI 合成）的上限。
//
// ⚠️ 起因：API 全局 30 秒超时，真实模型写一份分镜常常要一两分钟 —— 请求在模型写到一半时被掐断
//
//	（假上游秒回，测试里从没撞上过）。与 Provider 的整体请求超时同量级，由它先触发、带上准确的错误分类。
//
// ⚠️ 路由超时（cmd/api）与恢复扫描的判死阈值（staleCallAfter）都从这里取，别各写一个数。
const SyncLLMRequestTimeout = 15 * time.Minute

// staleCallAfter 同步模型调用停在 running 超过这么久就判死。
// ⚠️ 必须大于请求上限：否则会把一个还在等模型的调用判失败并退款，而它随后成功写进镜头 —— 付费产出白送。
const staleCallAfter = SyncLLMRequestTimeout + 5*time.Minute

const staleCallReason = "调用超时未结束（请求被中断或服务重启），已退款"

// RecoverStaleCalls 把停在 running 太久的同步模型调用判失败并退款，返回本次处理的条数。
//
// ⚠️ 起因：失败路径的退款与终态曾用已取消的请求 ctx 写库，一起失败 —— 钱扣了、调用永远停在 running，
//
//	而 abortCall 注释里说的「漏掉的由对账作业补」并不存在。进程在模型返回之前崩溃也是同一个结局。
//
// ⚠️ 顺序是**先退款、再判失败**：退款幂等（账本唯一索引兜底），判失败是 CAS。反过来的话，判完失败而退款没成，
//
//	下一轮就再也捞不到它了。多个 API 实例同时扫也安全。
func (s *Service) RecoverStaleCalls(ctx context.Context) (int, error) {
	calls, err := s.repo.StaleRunningCalls(ctx, staleCallAfter, 100)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range calls {
		if c.CreditsCost > 0 && s.ledger != nil {
			if err := s.ledger.Refund(ctx, c.PayerAccountID, "script_llm",
				strconv.FormatInt(c.ID, 10), staleCallReason); err != nil {
				s.warn(ctx, "恢复扫描退款失败，下一轮再试", "callId", c.ID, "error", err)
				continue
			}
		}
		changed, err := s.repo.FailCallIfRunning(ctx, c.ID, staleCallReason)
		if err != nil {
			s.warn(ctx, "恢复扫描写终态失败，下一轮再试", "callId", c.ID, "error", err)
			continue
		}
		if changed {
			n++
		}
	}
	return n, nil
}

// settleContext 给「模型已经返回（或失败）之后」的结账、落库、退款用：**脱离请求的取消**。
//
// ⚠️ 请求的 ctx 会因为超时或浏览器断开被取消。拿它去写库的话，失败路径上的退款与终态会一起失败 ——
// 钱扣了、调用永远停在 running（实测：全局 30 秒超时掐断分镜生成，四次调用 162 积分全部没退）。
// ⚠️ 必须在模型返回**之后**再建：建早了，它自己的计时会在等模型的那几分钟里先到期。
func settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
}

// abortCall 统一处理失败：退款 + 标 failed。
//
// ⚠️ 退款失败只告警不改结论：调用已经失败了，再把它翻成"成功"更糟。
// 退款的一次性由账本的唯一索引兜底；漏掉的（进程在这一步之前崩了）由 RecoverStaleCalls 补。
// ⚠️ 自己脱离请求的取消：调用方传进来的 ctx 很可能正是那个已经超时的请求。
func (s *Service) abortCall(ctx context.Context, callID, payerID int64, refID string,
	credits int, d domain.SettleDecision) {
	ctx, cancel := settleContext(ctx)
	defer cancel()
	if d.Refund && credits > 0 && s.ledger != nil {
		if err := s.ledger.Refund(ctx, payerID, "script_llm", refID, d.Reason); err != nil {
			s.warn(ctx, "同步 LLM 退款失败，需对账补偿",
				"callId", callID, "payerAccountId", payerID, "error", err)
		}
	}
	if err := s.repo.FinishCall(ctx, callID, d.Status, nil, d.Reason); err != nil {
		s.warn(ctx, "写 LLM 调用终态失败", "callId", callID, "error", err)
	}
}

// buildStoryboardPrompt 组装系统提示词与用户内容。
//
// ⚠️ **样板段落由代码拼、不让模型写**：让模型每次重写一千多字的固定版式，
// 一是每次都会漂移（改一个字下游行为就变），二是把输出撑长 ——
// 而输出越长越容易在 JSON 字符串里写出裸引号，整批作废。
// 模型只写真正要创作的部分。
func (s *Service) buildStoryboardPrompt(ctx context.Context, sc repo.Script,
	in StoryboardInput) (system, user string, err error) {
	system = storyboardFallbackSystem
	if s.prompts != nil {
		// 提示词外置在 `prompt_template`：运营可以调优而不发版。
		// ⚠️ 变量要**给全**（含可选的）：模板用到哪个占位符由运营决定，
		// 少给一个就整条渲染失败、回落到内置 —— 而那只是一条 warn。
		vars := map[string]string{
			"story": sc.RawInput, "shotCount": strconv.Itoa(in.ShotCount),
			"globalStyle": sc.GlobalStyle, "characterRefs": in.ExtraContext,
			"duration": "",
		}
		if resolved, e := s.prompts.RenderPrompt(ctx, "storyboard_gen.system",
			in.ModelCode, vars); e == nil && resolved != "" {
			system = resolved
		} else if e != nil {
			s.warn(ctx, "取分镜系统提示词失败，回落内置", "error", e)
		}
	}
	// 输出契约由代码固定追加，**不依赖运营在模板里写对**：
	// 少一个字段名下游就解析不出来，而那是要扣费之后才发现的。
	system += "\n\n" + storyboardOutputContract

	var b strings.Builder
	if sc.GlobalStyle != "" {
		fmt.Fprintf(&b, "全片风格：%s\n", sc.GlobalStyle)
	}
	if in.ShotCount > 0 {
		fmt.Fprintf(&b, "期望镜头数：%d\n", in.ShotCount)
	}
	if in.ExtraContext != "" {
		fmt.Fprintf(&b, "补充上下文：\n%s\n", in.ExtraContext)
	}
	// ⚠️ 标签别写「剧情原文」：用户只填了「三打白骨精」时，模型据此认定自己
	// 还没拿到原文，回一句「请提供具体剧情原文」—— 2026-10-14 实测，扣费、解析失败、退款。
	fmt.Fprintf(&b, "剧情：\n%s", sc.RawInput)
	return system, b.String(), nil
}

// storyboardOutputContract 是输出格式契约。
//
// ⚠️ 它描述的是**结构**，不是创作要求 —— 创作部分在 prompt_template 里，
// 运营可以调；结构在这里，改它等于改解析器，必须一起改。
//
// 「输入很短也要直接出 JSON」也写在这里而不是模板里：这是同步调用，没有人能回答
// 模型的反问，反问就等于一次解析失败。运营模板写成「依据原文」时这一条照样生效。
//
// 「描述里的实体写成 @名称」同样是格式约定而不是创作要求（CD-78）：工作台按 @ 高亮、给 @ 候选，
// 解析器在模型没给 entities 时也靠 @ 兜底。此前契约里漏了这一条，模型写出来的描述没有一个 @ ——
// 画面描述里资产不高亮、输入 @ 没有候选（老系统的分镜提示词一直有这条）。
//
// assets 的字段与逐类型的写法同样照老系统（CD-79）：资产描述按「描述 / 服装 / 特征 / 前背景 / 氛围」分段拼，
// 每段读哪个字段由这里约定 —— 字段名或含义一变，描述里就有一段是空的。此前只写了「外形 / 常态服装 / 识别特征」，
// 模型给的是一句话的短语，变身形态也没有拆开（「年轻女子」与「白骨精」成了两个不相干的角色）。
const storyboardOutputContract = `只输出一个 JSON 对象，不要任何解释文字或代码块围栏。结构：
{"globalStyle":"全片视觉风格，一句话",
 "shots":[{"shotNo":1,"description":"画面内容","durationSec":5,
           "shotSize":"景别","lighting":"光线","dialogue":"对白","soundEffect":"音效",
           "cameraMove":"运镜","startState":"本镜开始时的画面状态",
           "endState":"本镜结束时的画面状态",
           "entities":[{"name":"实体名","type":"character|scene|prop"}]}],
 "assets":[{"name":"实体名","type":"character|scene|prop","alias":"别称",
            "appearance":"外观","costume":"服装（仅角色）","features":"特征（角色、道具）",
            "layers":"前景与背景（仅场景）","mood":"氛围（仅场景）"}]}

要求：
- 给你的剧情可能是完整剧本，也可能只是一个标题、梗概或一句话创意。后者按公认的故事情节
  （知名作品）或合理的创作补全后再拆镜头。不要反问，不要索要原文或补充材料 ——
  没有人会回答，JSON 以外的任何输出都按失败处理。
- durationSec 取 1~60 的整数。
- description 里凡出现的角色、场景、道具一律写成 @名称（与 entities 里的 name 完全一致），
  不要用「一个人」「某物」这类匿名指代。
- entities 列出本镜出现的全部实体，与 description 里的 @名称 一一对应，一个都不能漏、也不要多给；
  name 只写实体本名，不带动作、修饰或助词（写「唐僧」不写「唐僧骑白马」）。
  type 只能是 character（有生命、能行动的个体，含人、神、妖、精怪、动物）、
  scene（地点与环境，人物在其中活动，不能被携带）、prop（可被持有、使用或转手的物件）。
  同一实体在不同镜头里必须用完全相同的 name 和 type。
- 变身、易容、伪装：角色以完全不同的外貌出现时，每一种外貌各算一个独立的 character，
  name 写「本体·形态」（如 白骨精·老妇），description 里的 @ 也写这个复合名；本体真身若也出场另算一个。
  判据是「观众这一镜看到的那张脸」：换脸换身形要拆，只换衣服或表情不拆。
- assets 是全片实体档案，每个实体只出现一次，name 与 type 必须和 entities 完全一致；
  把全部镜头的 entities 汇总去重，每一个都要有档案，戏份少的次要道具也不能省。字段按类型写：
  character：alias 别称（没有填 ""）；appearance 写身份、性别、外表年龄、身高、体型、
    发型发质发色、脸型、瞳色眼神、肤色、体毛；costume 写上衣、下装、鞋、帽、饰品，
    逐部位说明颜色与材质；features 写辨识细节，如五官特点、伤疤老茧、习惯性神态。
  scene：alias 别称；appearance 写室内外、空间尺度、开阔或封闭、地形与围合关系、大致范围；
    layers 分别写背景层与前景层各有什么；mood 写氛围基调。
  prop：alias 别称；appearance 写尺寸、形状、材质、颜色、表面纹样；
    features 写磨损、包浆、划痕、附着物等辨识细节。
  变身形态按 character 写满它自己的外貌，不要写「同本体」这类指代；alias 写明是谁所化。
- 档案只写**去掉临时状态后仍然成立**的稳定事实：当下的动作、表情、手里拿着什么都属于状态，
  不要写进去（「手持经卷」不是特征）。剧本没交代的年龄、身高等可以合理设定，但全片一致。
  每个字段都要写满、能独立读懂，不要只写名字或一句话，不要「同上」「参考前文」。
- startState / endState 描述这一镜的运动边界，用于衔接上下镜。`

// storyboardFallbackSystem 是内置兜底。
//
// ⚠️ 与 `prompt_template` 里的那一份是**两份**：改提示词要双写，
// 只改一边的话 —— 只改代码会被库里的旧行盖掉（库优先），
// 只改库则丢掉了库没配时的兜底。
const storyboardFallbackSystem = `你是专业分镜师。根据用户给出的剧情，拆成若干个可拍摄的镜头。
每个镜头要有明确的画面内容、景别与时长；镜头之间要连贯。
识别剧情里出现的角色、场景、道具，并为每个实体写一份稳定的外观档案。`
