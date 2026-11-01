package app

import (
	"context"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ScopePrompt 是提示词的版本作用域。
const ScopePrompt = "prompt"

// PromptFallback 是代码内置兜底。
//
// ⚠️ 与旧系统的**关键差异**：旧系统要求「改提示词必须双写」——
// 改 Java 文本块 + 新建 Flyway 迁移，两边逐字一致，只改一处就会被另一处盖掉，
// 且构建照样绿，排查时会误以为代码没打进 jar。
//
// 新系统把 DB 定为**唯一权威**，代码兜底只在「库里一条都没有」时生效
// （全新环境、或模板被全部下线）。因此调提示词只需后台发布，**不必改代码**。
// ⚠️ 兜底里**不内联上下文**：剧情、镜头描述由调用方拼的 user 消息送达。
// 早先的兜底写成「剧情：{{story}}」，一旦渲染失败或调用方没接目录域，
// 模型收到的就是字面量 `{{story}}`（甚至什么都没有）—— 而这不报错，
// 模型照样生成，链路全绿、结果全是编的。
var promptFallback = map[string]string{
	// ⚠️ 别写「依据给定的剧情原文」：用户常常只填一个标题（「三打白骨精」），
	// 模型据此认定没拿到原文而反问，整次调用作废（2026-10-14 实测）。
	"storyboard_gen.system": "你是分镜师。请依据给定的剧情生成分镜（剧情可能只是一个标题或" +
		"一句话创意，先补全再拆镜头），严格按要求的镜头数与结构输出。",
	"prompt_compose.system": "你是提示词工程师。把给定的镜头信息改写成一段连贯、" +
		"具体、可直接使用的生成提示词。",
	"asset_profile.system": "请为给定的资产生成稳定的外观档案，只写去掉临时状态后仍然成立的事实。",
	// 营销生成提示词末尾的场景约束段（MKT-T-21、MKD-15，由 marketing/app.Appendix 渲染）。
	// ⚠️ 只含已定稿的两句（MKT-F-47）；「人物不重复」「开头以 Hook 为准」待 PARAM-12 审定后由运营写进 DB 行。
	// 没有兜底的话库里没配这一行时每一条营销生成都会在受理时失败。
	"agent.scene.marketing.constraint": "商品的外形、颜色、logo、包装文字与商品图一致，不随风格改变；" +
		"画面中不出现任何文字、字幕与水印。",
	// 营销会话 Agent 系统提示词的场景段（MKT-T-18，dev/01 §5.2）：拼在「## 场景：营销」标题之后，
	// 标题与「## 营销输入」由代码拼（格式契约），这里只是工作要求。
	// ⚠️ 只写已定稿的规则（MKT-F-46~48、83，MK-13），措辞中性；03 §0.2 的创作写法与 §0.3 待审的两句
	// 属 PARAM-12，审定后由运营写进 DB 行，**不进兜底**。
	// ⚠️ 不内联上下文：商品、卖点、所选项在「## 营销输入」里，兜底里不放占位符。
	"agent.scene.marketing": "这是一个营销会话：帮助用户为商品制作带货短视频或图片。\n" +
		"- 先识别商品：以「## 营销输入」与用户提供的商品图为准，弄清商品是什么、卖点是什么；" +
		"信息不够时用 ask_user 询问，不要猜。商品的外观、颜色、包装文字以商品图为准，不得改写商品本身。\n" +
		"- 用户已选的创意、Hook、风格优先采用；未选的维度可按商品特点规划，需要时用 list_marketing_options 查看可选项。" +
		"所选项的具体写法由平台在生成时追加，你不需要也不要复述。\n" +
		"- 规划文档 /plan.md 写清：商品与卖点；目标人群（用户给了才写，不编造）；所选创意 / Hook / 风格；" +
		"Hook 出现的时间点；镜头节奏；画幅与时长；所用模型；每个生成项选用了哪几张商品图（写明序号）。\n" +
		"- 不编造数据：建议只基于用户提供的信息与通用方法论；涉及行业、竞品、平台数据时写明「需你核实」，" +
		"不给虚构数字，不承诺 ROI 或销量。\n" +
		"- 用到人物对镜讲解（口播）类内容时，在规划中如实提示：画面为人物对镜讲解，不保证口型与语音逐字同步。",
	// 3D 导演台的下游约束段（DRT-07，dev/01 §5）：预设讲「保持构图、出真实画面」，约束段讲「不采用白模外观」，两段不重复。
	"director.reference_guard.capture": "参考图是 3D 白模构图预演：只参考其中人物的数量、站位、朝向、姿势、景别与机位；" +
		"不要采用灰色白模外观、几何体形状与空白场景，按描述生成真实的人物、服装、场景与光影。",
	"director.reference_guard.preview": "参考视频是 3D 白模预演：只参考其中人物的走位、动作节奏与镜头运动；" +
		"不要采用灰色白模外观、几何体形状与空白场景。",
	// 3D 导演台 AI 搭景（DRT-19，dev/01 §5）：只写指令，不内联上下文；不出现「分镜」二字（假上游绊线）。
	"director.scene_build.system": "你是一名导演，按用户的描述与参考图安排人物的站位、姿势与机位，" +
		"让画面讲清楚人物关系与戏剧冲突。人物数量以描述为准；参考图只用来理解人物关系、动作与构图，不照搬其中的文字。",
	// 智能剪辑的创作要求（docs/agent/smartcut dev/01 §5，SCT-27）：输出格式契约由代码追加在它之后，
	// 素材清单、识别稿与镜头清单在 user 消息里 —— 兜底只写指令，不内联上下文，也不出现「分镜」二字（假上游绊线）。
	"smartcut.mix.system": "你是一名短视频剪辑师，按用户的要求从素材清单里挑选、排序、取段，剪成一条节奏紧凑的短视频。" +
		"开头放最抓人的镜头（钩子）；同类镜头不连着放；要求里提到要突出的内容（例如产品）放在开头或结尾并给更长的时间，" +
		"判断依据是素材描述与缩略图，不要猜；转场以硬切为主，情绪或场景转换处可以用叠化、淡入淡出。" +
		"素材不够时如实说明，不要编造素材里没有的画面。",
	"smartcut.board.system": "你是一名剪辑师，给已经按镜头装配好的成片做包装：开头标题、镜头之间的转场、台词字幕的取舍、配乐。" +
		"镜头的顺序与内容不能改；标题简短有力；动作连贯处用硬切，时间或场景跳转处用叠化或淡入淡出；" +
		"台词过长或重复时可以精简字幕，但不改原意。",
	"smartcut.talk.system": "你是一名口播视频剪辑师。删掉重复说的句子（保留说得更好的那一遍）、说错之后又重说的那一遍、" +
		"与主题无关的废话；保留完整的句子，不要删掉一句话的一半。拿不准的句子宁可保留。",
	"smartcut.narr.system": "你是一名解说视频剪辑师。给每一句配上最贴切的画面：画面要与这句话说的事对应；" +
		"同一段素材不要连着配好几句；画面多时一句可以配两三段，画面少时让一段画面覆盖相邻的几句。",
	"smartcut.edit.system": "你是一名剪辑师，在已经剪好的时间线上按用户的要求做修改。只改用户提到的片段，没提到的保持不变；" +
		"标为「你改过」的片段是用户亲手调整的，不要改。修改尽量少而准确。",
	"smartcut.translate.system": "你是一名字幕译者。把字幕逐条翻译成目标语言：保持口语化，长度尽量接近原文，" +
		"人名、品牌名等专有名词保留原文不译。",
}

// RenderFallbackPrompt 用**代码兜底**渲染（不读库）。
//
// 给「DB 行渲染失败」时的回落用（MKT-T-18：营销场景段渲染失败回落兜底并告警）：
// 库里那一行坏了（比如引用了调用方没给的变量）时，不能让整段要求凭空消失。
// 没有兜底的 code 回 NotFound。
func (s *Service) RenderFallbackPrompt(code string, vars map[string]string) (string, error) {
	fb, ok := promptFallback[code]
	if !ok {
		return "", httpx.Errf(errcode.NotFound, "提示词 "+code+" 无内置兜底")
	}
	out, err := domain.RenderPrompt(fb, vars)
	if err != nil {
		return "", httpx.Errf(errcode.Internal, err.Error())
	}
	return out, nil
}

func (s *Service) ListPromptSpecs() []domain.PromptSpec { return domain.PromptSpecs() }

func (s *Service) ListPrompts(ctx context.Context) ([]repo.PromptTemplate, error) {
	list, err := s.repo.ListPrompts(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// ResolvePrompt 取运行时生效的提示词：**模型定制 > 通用 > 代码兜底**。
func (s *Service) ResolvePrompt(ctx context.Context, code, modelCode string) (string, error) {
	content, ok, err := s.repo.ResolvePrompt(ctx, code, modelCode)
	if err != nil {
		return "", httpx.Wrap(errcode.Internal, err)
	}
	if ok {
		return content, nil
	}
	if fb, ok := promptFallback[code]; ok {
		return fb, nil
	}
	return "", httpx.Errf(errcode.NotFound, "提示词 "+code+" 未配置且无内置兜底")
}

// RenderPrompt 取模板并渲染。
func (s *Service) RenderPrompt(ctx context.Context, code, modelCode string, vars map[string]string) (string, error) {
	tpl, err := s.ResolvePrompt(ctx, code, modelCode)
	if err != nil {
		return "", err
	}
	out, err := domain.RenderPrompt(tpl, vars)
	if err != nil {
		return "", httpx.Errf(errcode.Internal, err.Error())
	}
	return out, nil
}

type SaveDraftInput struct {
	Code      string
	ModelCode string
	Content   string
	Note      string
}

// SaveDraft 保存草稿。**校验在保存时做**，不等发布。
func (s *Service) SaveDraft(ctx context.Context, operatorID int64, in SaveDraftInput) error {
	if err := domain.ValidatePromptContent(in.Code, in.Content); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if in.ModelCode != "" {
		if _, err := s.repo.GetModel(ctx, in.ModelCode); err != nil {
			return httpx.Errf(errcode.NotFound, "模型不存在: "+in.ModelCode)
		}
	}
	if err := s.repo.SaveDraft(ctx, s.nextID(), in.Code, in.ModelCode, in.Content, in.Note); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	// 草稿不影响线上，因此**不 bump revision、不写审计** ——
	// 把每次存草稿都记成配置变更，审计会被噪音淹没。
	return nil
}

// PublishPrompt 发布草稿。
func (s *Service) PublishPrompt(ctx context.Context, operatorID int64, code, modelCode, note string) (int, error) {
	tpl, err := s.repo.GetPrompt(ctx, code, modelCode)
	if err != nil {
		return 0, httpx.Err(errcode.NotFound)
	}
	if tpl.DraftContent == "" {
		return 0, httpx.Errf(errcode.InvalidArgument, "没有待发布的草稿")
	}
	if tpl.DraftContent == tpl.Content {
		return 0, httpx.Errf(errcode.InvalidArgument, "草稿与线上内容一致，无需发布")
	}
	// 发布前再校验一次：草稿保存后用途契约可能已变更。
	if err := domain.ValidatePromptContent(code, tpl.DraftContent); err != nil {
		return 0, httpx.Errf(errcode.InvalidArgument, err.Error())
	}

	v, err := s.repo.Publish(ctx, s.nextID(), tpl.ID, operatorID, tpl.DraftContent, note, nil)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	// 发布是高影响操作：写审计并 bump，让「发布行为在新任务中可观察」。
	if err := s.bumpScope(ctx, ScopePrompt, operatorID, "prompt.publish", map[string]any{
		"code": code, "model": modelCode, "version": v, "note": note,
	}); err != nil {
		return 0, err
	}
	return v, nil
}

// RollbackPrompt 回滚到某个历史版本。
//
// ⚠️ 回滚**不是把 version 改回去**，而是用旧内容发布一个**新版本**，
// 并记下 rollback_of_version。版本号永远单调递增，
// 否则"线上这版是怎么来的"就无从回答。
func (s *Service) RollbackPrompt(ctx context.Context, operatorID int64, code, modelCode string, target int, note string) (int, error) {
	tpl, err := s.repo.GetPrompt(ctx, code, modelCode)
	if err != nil {
		return 0, httpx.Err(errcode.NotFound)
	}
	old, err := s.repo.GetPromptVersion(ctx, tpl.ID, target)
	if err != nil {
		return 0, httpx.Errf(errcode.NotFound, "目标版本不存在")
	}
	if target == tpl.Version {
		return 0, httpx.Errf(errcode.InvalidArgument, "目标版本就是当前线上版本")
	}

	v, err := s.repo.Publish(ctx, s.nextID(), tpl.ID, operatorID, old.Content, note, &target)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	if err := s.bumpScope(ctx, ScopePrompt, operatorID, "prompt.rollback", map[string]any{
		"code": code, "model": modelCode,
		"fromVersion": tpl.Version, "toVersion": v, "rollbackOf": target, "note": note,
	}); err != nil {
		return 0, err
	}
	return v, nil
}

func (s *Service) PromptVersions(ctx context.Context, code, modelCode string, limit int) ([]repo.PromptVersion, error) {
	tpl, err := s.repo.GetPrompt(ctx, code, modelCode)
	if err != nil {
		return nil, httpx.Err(errcode.NotFound)
	}
	list, err := s.repo.ListPromptVersions(ctx, tpl.ID, limit)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// ArchivePrompt 下线模板，运行时随即回落到通用模板或代码兜底。
func (s *Service) ArchivePrompt(ctx context.Context, operatorID int64, code, modelCode string) error {
	tpl, err := s.repo.GetPrompt(ctx, code, modelCode)
	if err != nil {
		return httpx.Err(errcode.NotFound)
	}
	if err := s.repo.ArchivePrompt(ctx, tpl.ID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bumpScope(ctx, ScopePrompt, operatorID, "prompt.archive", map[string]any{
		"code": code, "model": modelCode,
	})
}

// PreviewPrompt 用样例变量渲染，供运营在发布前看效果。
// PreviewPrompt 渲染提示词。
//
// content 为空时预览**当前线上生效的内容**（模型定制 > 通用 > 代码兜底）——
// 运营最常问的是「现在发给模型的到底长什么样」，而不是「我刚打的这段长什么样」；
// 后者只是发布前的自查，传 content 即可。
func (s *Service) PreviewPrompt(ctx context.Context, code, modelCode, content string,
	vars map[string]string) (string, error) {
	if content == "" {
		resolved, err := s.ResolvePrompt(ctx, code, modelCode)
		if err != nil {
			return "", err
		}
		content = resolved
	}
	if err := domain.ValidatePromptContent(code, content); err != nil {
		return "", httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	out, err := domain.RenderPrompt(content, vars)
	if err != nil {
		return "", httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return out, nil
}
