//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/settings"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestSettings_默认值写入与回调(t *testing.T) {
	pool := testsupport.Fresh(t)
	ctx := context.Background()
	s, err := settings.Open(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if s.Int(settings.GenerationMaxRunning) != 4 || s.Bool(settings.UpdateAutoCheck) != true {
		t.Fatal("没设置过的键应取默认值")
	}
	var got any
	s.Watch(settings.GenerationMaxRunning, func(v any) { got = v })
	restart, err := s.Set(ctx, map[string]json.RawMessage{
		settings.GenerationMaxRunning: raw(8),
		settings.ProviderAllowPrivate: raw(true),
		settings.S3SecretKey:          raw("abcdefgh1234"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != 8 {
		t.Fatalf("回调应收到新值，得到 %v", got)
	}
	if len(restart) != 2 {
		t.Fatalf("出网开关与 S3 密钥要重启生效：%v", restart)
	}
	// 重新打开读库：值持久化了。
	s2, _ := settings.Open(ctx, pool)
	if s2.Int(settings.GenerationMaxRunning) != 8 || !s2.Bool(settings.ProviderAllowPrivate) ||
		s2.String(settings.S3SecretKey) != "abcdefgh1234" {
		t.Fatal("重新打开后值不对")
	}
	for _, it := range s2.List() {
		if it.Key == settings.S3SecretKey && it.Value != "****1234" {
			t.Fatalf("密钥只给掩码：%v", it.Value)
		}
	}
}

func TestSettings_未知键与越界整批拒绝(t *testing.T) {
	pool := testsupport.Fresh(t)
	ctx := context.Background()
	s, _ := settings.Open(ctx, pool)
	if _, err := s.Set(ctx, map[string]json.RawMessage{"generation.max_runing": raw(2)}); !errors.Is(err, settings.ErrUnknownKey) {
		t.Fatalf("拼错的键应被拒：%v", err)
	}
	if _, err := s.Set(ctx, map[string]json.RawMessage{
		settings.RequestLogRetention:  raw(10),
		settings.GenerationMaxRunning: raw(999),
	}); err == nil {
		t.Fatal("越界应整批拒绝")
	}
	if s.Int(settings.RequestLogRetention) != 30 {
		t.Fatal("整批拒绝时其他键也不应写入")
	}
	if _, err := s.Set(ctx, map[string]json.RawMessage{settings.UpdateAutoCheck: raw("yes")}); err == nil {
		t.Fatal("类型不对应被拒")
	}
}
