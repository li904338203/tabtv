package domain

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 后台保存模型 / 供应商 / 素材 / 音色时的字段校验（P15-05 补编辑表单时加）。
//
// ⚠️ 起因：此前服务层只判「必填」，而列是定长的 varchar、带 CHECK 约束 ——
// 超长的名称、不在枚举里的状态在写库时变成 500，运营看到「服务暂时不可用」，
// 而真正的原因是他多打了几个字。更糟的两类**不报错**：
//   - 模型的 domain 不在四种产出里：存得进去，之后能力筛选、计价下拉都找不到它；
//   - 供应商的自定义请求头不是「字符串 → 字符串」：运行时解析失败被**静默忽略**，
//     连带 `X-Auth-Header-Name` 一起丢掉，上游回 401 而配置页一切正常。

var catalogCodeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func checkCode(what, code string, max int) error {
	if !catalogCodeRe.MatchString(code) || len(code) > max {
		return fmt.Errorf("%s编码只能用小写字母、数字、下划线与连字符，以字母或数字开头，最长 %d 位", what, max)
	}
	return nil
}

// checkText 按**字符**数校验（PG 的 varchar(n) 是字符数，不是字节数）。
func checkText(what, s string, max int, required bool) error {
	if required && strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s不能为空", what)
	}
	if utf8.RuneCountInString(s) > max {
		return fmt.Errorf("%s不能超过 %d 个字", what, max)
	}
	return nil
}

// ModelDomains 是模型的四种产出。
var ModelDomains = []string{"text", "image", "video", "audio"}

// ModelFields 是保存模型时逐列校验的字段（列宽见 00004_catalog_model.sql）。
type ModelFields struct {
	Code, Name, Domain, SubType, UpstreamModelCode, SubmitPath, QueryPath, Status string
	MaxInputChars                                                                 *int
	EstSeconds                                                                    int
	// 视频能力（AGT-05）：nil = 本次不设置（保留库里的值）。只对 domain=video 有意义。
	MaxDurationSec              *int
	IsMultiShot, HasNativeAudio *bool
}

// MaxVideoDurationSec 是单次最长时长的上限（设计建议值，非已批准政策）。
// 超过十分钟的单段视频没有任何上游提供，填出来多半是把毫秒当成了秒。
const MaxVideoDurationSec = 600

func ValidateModelFields(f ModelFields) error {
	if err := checkCode("模型", f.Code, 64); err != nil {
		return err
	}
	if err := checkText("模型名称", f.Name, 64, true); err != nil {
		return err
	}
	ok := false
	for _, d := range ModelDomains {
		ok = ok || d == f.Domain
	}
	if !ok {
		return fmt.Errorf("模型类型只能是 text / image / video / audio")
	}
	for _, c := range []struct {
		what     string
		v        string
		max      int
		required bool
	}{
		{"子类型", f.SubType, 32, false},
		{"上游模型名", f.UpstreamModelCode, 128, true},
		{"提交路径", f.SubmitPath, 256, false},
		{"查询路径", f.QueryPath, 256, false},
	} {
		if err := checkText(c.what, c.v, c.max, c.required); err != nil {
			return err
		}
	}
	if f.Status != "enabled" && f.Status != "disabled" {
		return fmt.Errorf("状态只能是 enabled / disabled")
	}
	if f.MaxInputChars != nil && *f.MaxInputChars <= 0 {
		return fmt.Errorf("输入字数上限必须大于 0（留空表示不限）")
	}
	// est 决定超时墙 max(30, est×3)：一小时以上的估时多半是把毫秒当成了秒。
	if f.EstSeconds > 3600 {
		return fmt.Errorf("预计耗时不能超过 3600 秒")
	}
	return validateVideoTraits(f)
}

// validateVideoTraits 校验视频能力三项。
//
// ⚠️ 非视频模型**传了非默认值**才拒（传 false / 不传都放行）：
// 管理端切换类型时会把三项一并带上默认值，一律拒的话改一个图片模型的名字也存不进去。
// 拒的时候要点名字段 —— 一句「参数非法」运营不知道该去清哪一项。
func validateVideoTraits(f ModelFields) error {
	if f.Domain != "video" {
		switch {
		case f.MaxDurationSec != nil:
			return fmt.Errorf("maxDurationSec（单次最长时长）只对视频模型有意义，%s 模型不能设置", f.Domain)
		case f.IsMultiShot != nil && *f.IsMultiShot:
			return fmt.Errorf("isMultiShot（支持多镜头）只对视频模型有意义，%s 模型不能设置", f.Domain)
		case f.HasNativeAudio != nil && *f.HasNativeAudio:
			return fmt.Errorf("hasNativeAudio（自带音轨）只对视频模型有意义，%s 模型不能设置", f.Domain)
		}
		return nil
	}
	if d := f.MaxDurationSec; d != nil && (*d <= 0 || *d > MaxVideoDurationSec) {
		return fmt.Errorf("maxDurationSec（单次最长时长）必须在 1~%d 秒之间（留空表示未配置）", MaxVideoDurationSec)
	}
	return nil
}

// ProviderFields 是保存供应商时逐列校验的字段。
type ProviderFields struct {
	Code, Name, AdapterType, BaseURL, TestPath string
	Headers                                    []byte
}

func ValidateProviderFields(f ProviderFields) error {
	if err := checkCode("供应商", f.Code, 32); err != nil {
		return err
	}
	if err := checkText("供应商名称", f.Name, 64, true); err != nil {
		return err
	}
	if err := checkText("适配器类型", f.AdapterType, 32, true); err != nil {
		return err
	}
	if len(f.BaseURL) > 512 {
		return fmt.Errorf("地址不能超过 512 个字符")
	}
	u, err := url.Parse(f.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("地址必须是 http:// 或 https:// 开头的完整地址")
	}
	if err := checkText("测试路径", f.TestPath, 256, false); err != nil {
		return err
	}
	if len(f.Headers) > 0 {
		var h map[string]string
		if err := json.Unmarshal(f.Headers, &h); err != nil {
			return fmt.Errorf("自定义请求头必须是「名称 → 字符串值」的对象")
		}
		for k, v := range h {
			if strings.TrimSpace(k) == "" || len(k) > 128 || len(v) > 1024 {
				return fmt.Errorf("自定义请求头的名称不能为空且不超过 128 个字符，值不超过 1024 个字符")
			}
		}
	}
	return nil
}

var paramCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ValidateParamDefFields 校验参数定义的编码与名称。
//
// ⚠️ 编码就是节点参数在画布数据里的键（`params.aspect_ratio`）：混进大写、连字符或空格的话，
// 前端按它取值、后端按它校验，两边迟早对不上，而表现是「选了等于没选」。
func ValidateParamDefFields(code, name string) error {
	if !paramCodeRe.MatchString(code) {
		return fmt.Errorf("参数编码只能用小写字母开头，由小写字母、数字与下划线组成，最长 64 位")
	}
	return checkText("参数名称", name, 64, true)
}
