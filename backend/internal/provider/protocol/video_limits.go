package protocol

import (
	"fmt"
	"strings"
)

// VideoLimits 是视频协议对参数的硬性范围：时长、清晰度、画幅（04 文档 P-61）。
//
// ⚠️ 这些范围原先只写在各家 BuildSubmit 里，只有 Worker 构造请求时才检查。那时任务已经受理、
// 积分已经扣了，一条超出范围的视频要扣费、排队、失败、退款走一圈，
// Agent 白等一轮。实测一个营销会话一次提交 5 条 1~3 秒的视频，全部在 Worker 里失败。
// 现在 BuildSubmit 与报价共用这一份声明：报价时就能拒，而且两边不会对不上。
//
// ⚠️ 这是**协议层的硬限制**，不是型号能力。某个型号只支持 5/10 秒这类更细的限制，
// 仍然该由按型号下发的配置表达（model_param_support），不写进这里。
type VideoLimits struct {
	MinSeconds int
	MaxSeconds int
	// Resolutions 为空 = 不限（协议不带这个字段或原样透传）。
	Resolutions []string
	// Ratios 为空 = 不检查：该协议在这个操作下不带画幅，或带不带取决于请求的其它部分。
	Ratios []string
}

// VideoLimitsDeclaring 由能说清参数范围的视频协议实现。
type VideoLimitsDeclaring interface {
	// VideoLimits 返回某个操作的参数范围；ok=false 表示不声明（不检查）。
	VideoLimits(operation string) (VideoLimits, bool)
}

// VideoLimitsOf 取一个协议在某操作下的参数范围；协议没注册或没声明时 ok=false。
func VideoLimitsOf(protocolCode, operation string) (VideoLimits, bool) {
	p, ok := registry[protocolCode]
	if !ok {
		return VideoLimits{}, false
	}
	d, ok := p.(VideoLimitsDeclaring)
	if !ok {
		return VideoLimits{}, false
	}
	return d.VideoLimits(operation)
}

// CheckDuration 校验时长；0 按缺省 5 秒算（与计价的缺省一致）。
func (l VideoLimits) CheckDuration(sec int) error {
	if sec <= 0 {
		sec = DefaultVideoSeconds
	}
	if l.MinSeconds > 0 && sec < l.MinSeconds || l.MaxSeconds > 0 && sec > l.MaxSeconds {
		return fmt.Errorf("视频时长必须在 %d 到 %d 秒之间（当前 %d）", l.MinSeconds, l.MaxSeconds, sec)
	}
	return nil
}

// CheckParams 校验节点参数里的时长、清晰度与画幅（键名与 BuildSubmit 读的一致）。没给的不检查。
//
// ⚠️ 时长也要从参数里再查一遍：BuildSubmit 读的是 params.duration，调用方单独传的时长可能是 0
// （按缺省 5 秒报价），只查那一个的话参数里的 3 秒照样漏到 Worker。
func (l VideoLimits) CheckParams(params map[string]any) error {
	if l.MinSeconds > 0 || l.MaxSeconds > 0 {
		max := l.MaxSeconds
		if max <= 0 {
			max = int(^uint(0) >> 1)
		}
		if _, err := durationOf(params, l.MinSeconds, max); err != nil {
			return err
		}
	}
	if len(l.Resolutions) > 0 {
		if _, err := resolutionOf(params, l.Resolutions, ""); err != nil {
			return err
		}
	}
	if len(l.Ratios) > 0 {
		if _, err := ratioOf(params, l.Ratios); err != nil {
			return err
		}
	}
	return nil
}

// fixedRatios 是可选的固定画幅（去掉 adaptive）。
func (l VideoLimits) fixedRatios() []string {
	var rs []string
	for _, r := range l.Ratios {
		if r != "adaptive" {
			rs = append(rs, r)
		}
	}
	return rs
}

// Describe 一句话说明范围（写进 Agent 的系统提示词）。
func (l VideoLimits) Describe() string {
	var parts []string
	if l.MinSeconds > 0 || l.MaxSeconds > 0 {
		parts = append(parts, fmt.Sprintf("单次时长 %d~%d 秒", l.MinSeconds, l.MaxSeconds))
	}
	if len(l.Resolutions) > 0 {
		parts = append(parts, "清晰度 "+strings.Join(l.Resolutions, " / "))
	}
	if len(l.Ratios) > 0 {
		parts = append(parts, "画幅 "+strings.Join(l.fixedRatios(), " / "))
	}
	return strings.Join(parts, "；")
}

// 各家协议共用的取值（与原先写在 BuildSubmit 里的逐字一致）。
var (
	commonVideoResolutions = []string{"480p", "720p", "1080p", "2k", "4k"}
	commonVideoRatios      = []string{"adaptive", "16:9", "9:16", "1:1", "4:3", "3:4", "21:9"}
)
