package app

import (
	"context"
	"log/slog"
	"sort"
)

// 活动引用的**计算式**判定（P14-03）。
//
// ⚠️ 为什么不继续用 `media_usage`：那是一张要各域**主动登记**的表，
// 而登记漏掉任何一处**都不会有编译错误**。实测结果是八个 `UsageType` 常量里
// 只有两个真的有人登记（`project_cover`、`generation_output`）——
// 脚本资产、时间线片段、生成输入一条都没有过。于是「这个文件没人用」
// 对几乎每一个素材都成立，而那正是删除保护要回答的那个问题。
//
// 这与提示词的 `ready`/`stale` 是同一条（CD-36）：**标记式**的正确性依赖
// "每个改动点都记得改"，**计算式**不依赖任何人的记性。所以这里反过来 ——
// 删除时**当场去问各域**「你还在用它吗」。新加一个引用媒体的域时，
// 忘了实现这个端口的表现是"少了一条保护"，而**实现了就永远不会失效**。
//
// `media_usage` 表**保留**：它仍然承担"短命的、跨进程的占用"（进行中的生成
// 任务在 Worker 那侧持有输入），那种引用没有一张可查的业务表。

// Reference 是一条活动引用。
type Reference struct {
	// Kind 引用方的类型，用于给用户的那句话（"正被时间线片段引用"）。
	Kind string
	// RefID 引用方的业务 ID，排障时用。
	RefID int64
}

// ReferenceChecker 由各引用媒体的域实现：回答「这个媒体现在还有谁在用」。
//
// ⚠️ 实现必须是**当场查业务表**，不能查缓存也不能查 `media_usage` ——
// 那样就退回成标记式了。
//
// ⚠️ 查询要**跨工作区**：共享导入复制的是字节（CD-57），所以同一个
// media_object 原则上只属于一个工作区；但派生件、脚本资产这些都可能在
// 别的地方挂着，按工作区过滤会漏掉引用 —— 而漏掉的后果是把还在用的文件删了。
type ReferenceChecker interface {
	// Kind 这个实现代表哪一类引用（用于文案）。
	Kind() string
	// References 返回引用该媒体的业务对象 ID。没有就回空。
	References(ctx context.Context, mediaID int64) ([]int64, error)
}

// AddReferenceChecker 注册一个引用来源。在 `cmd/api` / `cmd/worker` 组装时调用。
//
// ⚠️ 两个进程都要注册：API 负责"删不掉"，Worker 的回收器负责"收不走"。
// 只注册一边的话，API 拒绝删除而回收器照样把它收掉 —— 两个都"没错"。
func (s *Service) AddReferenceChecker(c ReferenceChecker) {
	if c == nil {
		return
	}
	s.checkers = append(s.checkers, c)
}

// liveReferences 汇总所有来源的活动引用。
//
// ⚠️ **任何一个来源查询失败都按「有引用」处理**，不是按「没有」。
// 反过来的话，一次数据库抖动会让删除保护整条失效 —— 而删除是不可逆的。
// 这与水印默认值同向：拿不准时倒向"看得见的那一侧"。
func (s *Service) liveReferences(ctx context.Context, mediaID int64) ([]Reference, error) {
	var out []Reference
	for _, c := range s.checkers {
		ids, err := c.References(ctx, mediaID)
		if err != nil {
			if s.log != nil {
				s.log.Error("查活动引用失败，按「有引用」处理以免误删",
					"kind", c.Kind(), "mediaId", mediaID, "error", err)
			}
			return nil, err
		}
		for _, id := range ids {
			out = append(out, Reference{Kind: c.Kind(), RefID: id})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].RefID < out[j].RefID
	})
	return out, nil
}

// SetLogger 给媒体服务一个 logger（引用检查失败要能看见）。
func (s *Service) SetLogger(l *slog.Logger) { s.log = l }

// describeReferences 把引用列表说成一句人话。
//
// ⚠️ 要说清**是什么在用**，不是一句"该文件正在使用中"：用户删不掉一张图时，
// 他需要知道去哪里解除占用。同一类只说一次（一条素材被十个片段引用时，
// 列十遍没有任何额外信息）。
func describeReferences(refs []Reference) string {
	label := map[string]string{
		UsageProjectCover:  "项目封面",
		UsageGenerationIn:  "进行中的生成任务",
		UsageGenerationOut: "生成产物",
		UsageScriptAsset:   "脚本资产",
		UsageTimelineClip:  "时间线 / 智能剪辑",
		UsageMaterialCover: "素材封面",
		UsageVoiceSample:   "音色试听",
		// MKT-T-23：会话附件 / 营销商品图、角色图、源视频。
		UsageAgentSession:   "Agent 会话",
		UsageAgentMarketing: "营销会话",
		// MKT-T-33：商品库里未删除商品的图。
		UsageMarketingProduct: "营销商品",
		// MKT-T-34：角色库里未删除角色的图片与参考音频。
		UsageMarketingCharacter: "营销角色",
		// 3D 导演台：场景引用的全景 / 泼溅、截图与预演记录。
		UsageDirectorScene: "3D 导演台",
	}
	seen := map[string]bool{}
	var parts []string
	for _, r := range refs {
		l := label[r.Kind]
		if l == "" {
			l = r.Kind
		}
		if !seen[l] {
			seen[l] = true
			parts = append(parts, l)
		}
	}
	if len(parts) == 0 {
		return "其他资源"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "、" + p
	}
	return out
}
