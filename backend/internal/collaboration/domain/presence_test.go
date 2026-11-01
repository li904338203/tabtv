package domain

import (
	"errors"
	"testing"
	"time"
)

func TestValidateClientID(t *testing.T) {
	if err := ValidateClientID("tab-abc123"); err != nil {
		t.Fatalf("正常标识应通过：%v", err)
	}
	if err := ValidateClientID("  "); !errors.Is(err, ErrEmptyClientID) {
		t.Fatalf("空白应被拒，得到 %v", err)
	}
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateClientID(string(long)); !errors.Is(err, ErrClientIDTooLong) {
		t.Fatalf("超长应被拒，得到 %v", err)
	}
}

func TestMergeOnlineGroupsByUser(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	entries := []PresenceEntry{
		{UserID: 1, Nickname: "小明", ClientID: "a", SeenAt: now.Add(-2 * time.Second)},
		{UserID: 1, Nickname: "小明", ClientID: "b", SeenAt: now.Add(-1 * time.Second)},
		{UserID: 2, Nickname: "小红", ClientID: "c", SeenAt: now.Add(-5 * time.Second)},
	}
	got := MergeOnline(entries, nil, 2, now)
	if len(got) != 2 {
		t.Fatalf("三个端两个人，应合并成 2 条，得到 %d", len(got))
	}
	// 自己排最前。
	if !got[0].IsSelf || got[0].UserID != 2 {
		t.Fatalf("自己应排最前，得到 %+v", got[0])
	}
	// 同一个人开两个标签页要记成 2 个端 —— 用户看到"有人改了画布"时，
	// 这个数字直接回答了"会不会是我自己"。
	if got[1].Clients != 2 {
		t.Fatalf("小明应有 2 个端，得到 %d", got[1].Clients)
	}
	// seenAt 取最近的一次。
	if !got[1].SeenAt.Equal(now.Add(-1 * time.Second)) {
		t.Fatalf("应取最近一次心跳，得到 %v", got[1].SeenAt)
	}
}

// 靠"离开时删掉"的话，进程被杀/浏览器崩溃/网络直接断开都不会发来离开事件。
func TestMergeOnlineDropsExpired(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	entries := []PresenceEntry{
		{UserID: 1, Nickname: "在线", SeenAt: now.Add(-PresenceTTL + time.Second)},
		{UserID: 2, Nickname: "早走了", SeenAt: now.Add(-PresenceTTL - time.Second)},
	}
	got := MergeOnline(entries, nil, 0, now)
	if len(got) != 1 || got[0].UserID != 1 {
		t.Fatalf("过期的应被剔除，得到 %+v", got)
	}
}

// 成员被移出工作区之后，Redis 里的记录还在 —— 显示一个已经进不来的人在线，
// 比不显示更糟：别人会以为他正在改。
func TestMergeOnlineFiltersNonMembers(t *testing.T) {
	now := time.Now()
	entries := []PresenceEntry{
		{UserID: 1, Nickname: "在职", SeenAt: now},
		{UserID: 9, Nickname: "已被移除", SeenAt: now},
	}
	got := MergeOnline(entries, map[int64]bool{1: true}, 0, now)
	if len(got) != 1 || got[0].UserID != 1 {
		t.Fatalf("非成员应被剔除，得到 %+v", got)
	}
	// nil 表示不过滤（调用方已保证成员资格）。
	if all := MergeOnline(entries, nil, 0, now); len(all) != 2 {
		t.Fatalf("nil 应表示不过滤，得到 %d", len(all))
	}
}

// 顺序每次刷新都变的话，用户没法扫一眼就知道"人还是那几个"。
func TestMergeOnlineOrderIsStable(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	entries := []PresenceEntry{
		{UserID: 5, Nickname: "E", SeenAt: now},
		{UserID: 3, Nickname: "C", SeenAt: now},
		{UserID: 4, Nickname: "D", SeenAt: now},
	}
	first := MergeOnline(entries, nil, 0, now)
	for i := 0; i < 30; i++ {
		got := MergeOnline(entries, nil, 0, now)
		for j := range got {
			if got[j].UserID != first[j].UserID {
				t.Fatalf("第 %d 次顺序变了：%v vs %v", i, got, first)
			}
		}
	}
	// 同一时刻按 userID 兜底排序。
	if first[0].UserID != 3 || first[2].UserID != 5 {
		t.Fatalf("同 seenAt 应按 userID 排，得到 %+v", first)
	}
}

func TestMergeOnlineFillsMissingNickname(t *testing.T) {
	now := time.Now()
	got := MergeOnline([]PresenceEntry{
		{UserID: 1, Nickname: "", SeenAt: now},
		{UserID: 1, Nickname: "小明", SeenAt: now},
	}, nil, 0, now)
	if len(got) != 1 || got[0].Nickname != "小明" {
		t.Fatalf("空昵称应被非空的替换，得到 %+v", got)
	}
}

// 心跳必须明显小于 TTL：丢一次不该让人消失（网络抖动很常见），丢两次才该消失。
func TestHeartbeatIsWellUnderTTL(t *testing.T) {
	if PresenceHeartbeat*2 >= PresenceTTL {
		t.Fatalf("心跳 %v 相对 TTL %v 太稀：丢一次心跳就会让人从列表里消失",
			PresenceHeartbeat, PresenceTTL)
	}
	if PresenceHeartbeat*4 < PresenceTTL {
		t.Fatalf("心跳 %v 相对 TTL %v 太密：白白打请求，而离开要等很久才反映出来",
			PresenceHeartbeat, PresenceTTL)
	}
}
