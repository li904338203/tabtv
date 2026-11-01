package domain

import (
	"fmt"
	"strings"
)

// 商品库（MKT-T-33；02 MK-22、MKT-F-93、MKT-D-50；dev/05 MKD-34~37）。
//
// ⚠️ 校验只在这里一处：服务端保存、（将来）前端预检都按这份规则。张数上限是配置值，
// 由调用方传进来（ProductLimits），这里不读环境变量 —— 纯函数才能表驱动地把边界盖全。
// ⚠️ 本文件只管「字段本身合不合法」。图片**归属与格式**要查媒体域，在 app 层判
// （AllowedImageMime 是那一步用的白名单）。

// AllowedImageMimes 是商品 / 角色图片接受的格式（按媒体库里记录的、来自文件头嗅探的 content_type）。
//
// ⚠️ GIF 不收：动图当商品主图，上游多数只取第一帧，用户看到的与模型拿到的不一致（MKD-35）。
// BMP 在上传时就读不出来（MKT-PARAM-20）。不信客户端声明的 MIME。
var AllowedImageMimes = []string{"image/png", "image/jpeg", "image/webp"}

// AllowedImageMime 判断一个 content_type 能不能当商品 / 角色图。
//
// 只比主类型 + 子类型（忽略 `; charset=` 之类的参数与大小写）。
func AllowedImageMime(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	for _, m := range AllowedImageMimes {
		if ct == m {
			return true
		}
	}
	return false
}

// ProductLimits 是商品库的两个配置值（SEEAITV_MARKETING_MAX_PRODUCT_IMAGES / _MAX_PRODUCTS）。
type ProductLimits struct {
	// MaxImages 每个商品的图片上限。
	MaxImages int
	// MaxProducts 每个工作区未删除商品数上限（配额）。
	MaxProducts int
}

// Normalize 把 ≤ 0 的值换成默认值。
//
// ⚠️ 0 在这里没有「不限」的含义（0 张图的商品存不进去、配额 0 等于禁止新建）；
// config.Validate 会在启动时拒绝坏值，这里只是给未装配的调用方（测试、旧入口）一个确定的数。
func (l ProductLimits) Normalize() ProductLimits {
	if l.MaxImages <= 0 {
		l.MaxImages = DefaultMaxProductImages
	}
	if l.MaxProducts <= 0 {
		l.MaxProducts = DefaultMaxProducts
	}
	return l
}

// ProductInput 是新建 / 整份更新商品时的内容（ID 已由调用方解析成 int64）。
type ProductInput struct {
	Name          string
	SellingPoints string
	Description   string
	// ImageMediaIDs 有序，[0] 为主图。
	ImageMediaIDs []int64
}

// Normalized 去掉名称两端空白（卖点 / 描述原样保留：换行与缩进可能是用户有意写的）。
func (p ProductInput) Normalized() ProductInput {
	p.Name = strings.TrimSpace(p.Name)
	return p
}

// ValidateProduct 校验商品内容（MKT-F-93、MKT-AC-32）。
//
// ⚠️ **一次报完**：张数、名称、卖点、描述、重复图片各报各的，Field 是请求体里的字段路径
// （`imageMediaIds`、`imageMediaIds[2]`、`name`…），前端据此标红对应表单项。
// 调用前先 Normalized()。
func ValidateProduct(in ProductInput, lim ProductLimits) []Issue {
	lim = lim.Normalize()
	var out []Issue
	switch n := len(in.ImageMediaIDs); {
	case n == 0:
		out = append(out, errIssue("imageMediaIds", IssueRequired, "请至少上传 1 张商品图"))
	case n > lim.MaxImages:
		out = append(out, errIssue("imageMediaIds", IssueTooMany,
			fmt.Sprintf("商品图最多 %d 张，当前 %d 张", lim.MaxImages, n)))
	}
	seen := make(map[int64]int, len(in.ImageMediaIDs))
	for i, id := range in.ImageMediaIDs {
		field := fmt.Sprintf("imageMediaIds[%d]", i)
		if id <= 0 {
			out = append(out, errIssue(field, IssueBadValue, fmt.Sprintf("第 %d 张图片的 ID 非法", i+1)))
			continue
		}
		if j, dup := seen[id]; dup {
			// 同一张图出现两次：用户多半是拖拽时手滑，存进去的话「共 N 张」比实际多。
			out = append(out, errIssue(field, IssueDuplicate,
				fmt.Sprintf("第 %d 张与第 %d 张是同一张图片", i+1, j+1)))
			continue
		}
		seen[id] = i
	}
	switch n := RuneLen(in.Name); {
	case n == 0:
		out = append(out, errIssue("name", IssueRequired, "请填写商品名称"))
	case n > MaxProductName:
		out = append(out, tooLong("name", "商品名称", n, MaxProductName))
	}
	if n := RuneLen(in.SellingPoints); n > MaxSellingPoints {
		out = append(out, tooLong("sellingPoints", "商品卖点", n, MaxSellingPoints))
	}
	if n := RuneLen(in.Description); n > MaxProductDescription {
		out = append(out, tooLong("description", "商品描述", n, MaxProductDescription))
	}
	return out
}

// MainImage 是主图（有序列表的第一张）；没有图时为 0。
func MainImage(ids []int64) int64 {
	if len(ids) == 0 {
		return 0
	}
	return ids[0]
}

// CharacterNameForbidden 是角色名（工作区角色、营销角色共用）里不允许出现的字符（MKD-36）。
//
// ⚠️ Agent 规划与描述里按 `@名称` 称呼角色（CD-78）：`@` 出现在名字里会把一个角色名切成两段；
// 其余是文件名 / 标记语言里的危险字符，全角括号与半角同一理由。
// 导出给 `/marketing/config` 下发（MKT-T-37）—— 前端预检读它，不另抄一份（抄的话两边迟早差一个字符）。
const CharacterNameForbidden = "@<>/\\|:*?()（）"

// CharacterNameIssue 校验角色名（1~MaxCharacterName 码点、不含禁用字符），合法时返回 nil。
//
// 角色库（MKT-T-34）与营销角色（MKT-T-36）**共用这一处**；调用前先 TrimSpace。
// 重名要查库（工作区内 / 全局唯一索引），不在这里。
func CharacterNameIssue(field, name string) []Issue {
	switch n := RuneLen(name); {
	case n == 0:
		return []Issue{errIssue(field, IssueRequired, "请填写角色名称")}
	case n > MaxCharacterName:
		return []Issue{tooLong(field, "角色名称", n, MaxCharacterName)}
	}
	if i := strings.IndexAny(name, CharacterNameForbidden); i >= 0 {
		r := []rune(name[i:])[0]
		return []Issue{errIssue(field, IssueBadFormat,
			fmt.Sprintf("角色名称不能包含「%c」（不能包含 %s）", r, CharacterNameForbidden))}
	}
	return nil
}
