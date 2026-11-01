package app

import (
	"testing"
	"time"
)

func TestStatsRange(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	ptr := func(t time.Time) *time.Time { return &t }
	cases := []struct {
		name     string
		from, to *time.Time
		wantFrom time.Time
		wantTo   time.Time
		adjusted bool
	}{
		{"缺省近 7 天", nil, nil, now.Add(-7 * day), now, false},
		{"只给 to", nil, ptr(now.Add(-day)), now.Add(-8 * day), now.Add(-day), false},
		{"倒置交换", ptr(now), ptr(now.Add(-2 * day)), now.Add(-2 * day), now, true},
		{"超过 30 天夹到 30 天", ptr(now.Add(-90 * day)), ptr(now), now.Add(-30 * day), now, true},
		{"恰好 30 天不算校正", ptr(now.Add(-30 * day)), ptr(now), now.Add(-30 * day), now, false},
	}
	for _, c := range cases {
		f, to, adj := statsRange(now, c.from, c.to)
		if !f.Equal(c.wantFrom) || !to.Equal(c.wantTo) || adj != c.adjusted {
			t.Errorf("%s：got [%v, %v) adjusted=%v", c.name, f, to, adj)
		}
	}
}

func TestRevoker_Scope(t *testing.T) {
	if s, err := (Revoker{Kind: RevokerUser, ActorID: 3}).scope(); err != nil || s.UserID != 3 || s.WorkspaceID != 0 {
		t.Fatalf("用户范围：%+v %v", s, err)
	}
	if s, err := (Revoker{Kind: RevokerWorkspace, ActorID: 3, WorkspaceID: 9}).scope(); err != nil || s.UserID != 0 || s.WorkspaceID != 9 {
		t.Fatalf("工作区范围不按人：%+v %v", s, err)
	}
	if _, err := (Revoker{Kind: RevokerWorkspace, ActorID: 3}).scope(); err == nil {
		t.Fatal("工作区撤销缺工作区必须报错（否则退化成全站范围）")
	}
	if s, err := (Revoker{Kind: RevokerAdmin, ActorID: 1}).scope(); err != nil || s.UserID != 0 || s.WorkspaceID != 0 {
		t.Fatalf("平台范围：%+v %v", s, err)
	}
	if _, err := (Revoker{Kind: "x"}).scope(); err == nil {
		t.Fatal("未知发起方必须报错")
	}
}
