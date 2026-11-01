package domain

import (
	"errors"
	"strings"
	"testing"
)

func shots(n int) []Shot {
	out := make([]Shot, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Shot{ID: int64(i * 10), ShotNo: i, DurationSec: 5})
	}
	return out
}

func nos(list []Shot) []int {
	out := make([]int, 0, len(list))
	for _, s := range list {
		out = append(out, s.ShotNo)
	}
	return out
}

func ids(list []Shot) []int64 {
	out := make([]int64, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out
}

func TestReorder_上下移动后镜号连续(t *testing.T) {
	// ⚠️ 镜号必须连续且唯一：它进唯一索引 (script_id, shot_no)，
	// 也是用户口头交流的编号（"第 7 镜再来一版"）。留空号两边都会乱。
	got, err := Reorder(shots(3), 2, MoveUp)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{10, 30, 20}; !equalIDs(ids(got), want) {
		t.Fatalf("顺序不对: %v", ids(got))
	}
	for i, n := range nos(got) {
		if n != i+1 {
			t.Fatalf("镜号应连续: %v", nos(got))
		}
	}
}

func TestReorder_到头了原样返回而不是报错(t *testing.T) {
	// 用户连点两下上移，不该收到一个错误弹窗。
	got, err := Reorder(shots(3), 0, MoveUp)
	if err != nil {
		t.Fatalf("已在最上面应原样返回: %v", err)
	}
	if !equalIDs(ids(got), []int64{10, 20, 30}) {
		t.Fatalf("不该改动: %v", ids(got))
	}
	if _, err := Reorder(shots(3), 2, MoveDown); err != nil {
		t.Fatalf("已在最下面应原样返回: %v", err)
	}
}

func TestDuplicate_副本不继承提示词状态(t *testing.T) {
	// ⚠️ 提示词是按镜头内容算出来的。原样继承会让副本一上来就是 ready，
	// 而它其实从没被合成过 —— 用户拿它去出图，出的是上一个镜头的画面。
	src := shots(1)
	src[0].Description = "原镜头"
	got, err := Duplicate(src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应插入一个副本: %d", len(got))
	}
	if got[1].ID != 0 {
		t.Fatal("副本必须是新行（ID 为 0 待分配）")
	}
	if got[1].Version != 1 {
		t.Fatalf("副本版本应从 1 起: %d", got[1].Version)
	}
	if got[1].Description != "原镜头" {
		t.Fatal("内容该继承")
	}
}

func TestInsertAfter_到上限拒绝(t *testing.T) {
	if _, err := InsertAfter(shots(MaxShots), MaxShots-1, Shot{DurationSec: 5}); err == nil {
		t.Fatalf("超过 %d 镜应被拒", MaxShots)
	} else if !errors.Is(err, ErrTooManyShots) {
		t.Fatalf("错误类型不对: %v", err)
	}
}

func TestInsertAfter_插到最前面(t *testing.T) {
	got, err := InsertAfter(shots(2), -1, Shot{DurationSec: 3, Description: "新"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Description != "新" || got[0].ShotNo != 1 {
		t.Fatalf("应插到最前并成为第 1 镜: %+v", got[0])
	}
}

func TestInsertAfter_时长非法被拒(t *testing.T) {
	if _, err := InsertAfter(shots(1), 0, Shot{DurationSec: 0}); !errors.Is(err, ErrBadDuration) {
		t.Fatalf("时长 0 应被拒: %v", err)
	}
	if _, err := InsertAfter(shots(1), 0, Shot{DurationSec: 61}); !errors.Is(err, ErrBadDuration) {
		t.Fatalf("时长 61 应被拒: %v", err)
	}
}

func TestRemove_重排镜号(t *testing.T) {
	got, err := Remove(shots(3), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(ids(got), []int64{10, 30}) {
		t.Fatalf("删错了: %v", ids(got))
	}
	if !equalInts(nos(got), []int{1, 2}) {
		t.Fatalf("镜号该重排成 1,2: %v", nos(got))
	}
}

func TestValidateRawInput_按字符不按token(t *testing.T) {
	// ⚠️ 调用方手上只有字符串，token 数依分词器而异；不同模型上限差一个数量级。
	// 写死在 DTO 上的后果是换个长上下文模型也用不了。
	if err := ValidateRawInput("", 100); !errors.Is(err, ErrEmptyInput) {
		t.Fatalf("空输入应被拒: %v", err)
	}
	if err := ValidateRawInput("一二三", 0); err != nil {
		t.Fatalf("上限 0 表示不限: %v", err)
	}
	// 三个汉字是 3 个字符、9 个字节 —— 按字节算会误判。
	if err := ValidateRawInput("一二三", 3); err != nil {
		t.Fatalf("3 个汉字不该超过 3 字上限: %v", err)
	}
	err := ValidateRawInput("一二三四", 3)
	if !errors.Is(err, ErrInputTooLong) {
		t.Fatalf("超限应被拒: %v", err)
	}
	if !strings.Contains(err.Error(), "4") || !strings.Contains(err.Error(), "3") {
		t.Fatalf("要说清楚当前多少、上限多少: %v", err)
	}
}

func TestCanAdvanceTo_服务端独立校验(t *testing.T) {
	// ⚠️ 前端也会挡一道，但那是体验；直接打 API 绕过前端是最容易发生的事。
	if err := CanAdvanceTo(StepAssets, 0, true); err == nil {
		t.Fatal("没有镜头不能进资产步")
	}
	if err := CanAdvanceTo(StepPrompts, 3, false); err == nil {
		t.Fatal("资产没就绪不能进提示词步")
	}
	if err := CanAdvanceTo(StepPrompts, 3, true); err != nil {
		t.Fatalf("条件齐了该放行: %v", err)
	}
	// 退回第一步永远允许：用户改主意要能回去。
	if err := CanAdvanceTo(StepShots, 0, false); err != nil {
		t.Fatalf("回到第一步应永远允许: %v", err)
	}
}

func TestValidateSourceType(t *testing.T) {
	for _, s := range []SourceType{SourceScript, SourceCharacter, SourceManual} {
		if err := ValidateSourceType(s); err != nil {
			t.Errorf("%s 应合法: %v", s, err)
		}
	}
	if err := ValidateSourceType("novel"); !errors.Is(err, ErrBadSourceType) {
		t.Fatalf("未知来源应被拒: %v", err)
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
