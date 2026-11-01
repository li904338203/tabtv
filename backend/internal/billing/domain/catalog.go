package domain

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 商品目录（等级 / 套餐 / 积分包）的字段校验（P15-05 编辑表单补齐时加）。
//
// ⚠️ 起因：此前服务层只判「不能为空」，而列是 `varchar(32)` / `varchar(64)` / `integer` ——
// 超长的编码、名称或积分会在写库时变成 500，运营看到的是「服务暂时不可用」，
// 而真正的原因是他多打了几个字。

var catalogCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// MaxCatalogCredits 是积分包积分与套餐赠分的上限。列是 integer；而一次生成动辄
// 几十上百积分，到这个量级只可能是手滑多打了几个零。
const MaxCatalogCredits = 100_000_000

var (
	ErrCatalogCode    = errors.New("编码只能用小写字母开头，由小写字母、数字与下划线组成，最长 32 位")
	ErrCatalogName    = errors.New("名称不能为空，且不超过 64 个字")
	ErrCatalogCredits = errors.New("积分超出上限（1 亿）")
)

// ValidateCatalogCode 校验等级 / 积分包的编码。
//
// 编码是**业务键**（保存按它 upsert），也会出现在会员记录与订单快照里：
// 允许空格、大写或中文的话，「basic」与「Basic 」就是两个等级。
func ValidateCatalogCode(code string) error {
	if !catalogCodeRe.MatchString(code) {
		return ErrCatalogCode
	}
	return nil
}

// ValidateCatalogName 校验名称：按**字符**数，不按字节（`varchar(64)` 在 PG 里是字符数）。
func ValidateCatalogName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" || utf8.RuneCountInString(n) > 64 {
		return ErrCatalogName
	}
	return nil
}

// ValidateCatalogCredits 校验积分上限（下限由各自的规则管：积分包要 > 0，赠分可以是 0）。
func ValidateCatalogCredits(n int) error {
	if n > MaxCatalogCredits {
		return ErrCatalogCredits
	}
	return nil
}
