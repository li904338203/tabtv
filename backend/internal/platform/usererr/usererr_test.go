package usererr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFromUpstream_分类且绝不原样返回(t *testing.T) {
	cases := map[string]string{
		`上游返回 HTTP 400: {"error":{"message":"非常抱歉，该提示可能违反了我们的内容政策"}}`:     ContentPolicy,
		`上游内容审核未通过，请调整描述（Your request was rejected by the safety system）`: ContentPolicy,
		`上游返回 HTTP 429: {"error":"rate limit"}`:                           RateLimited,
		`上游返回 HTTP 401: Unauthorized`:                                     Misconfigured,
		`上游返回 HTTP 503: upstream connect error`:                           Unavailable,
		`上游返回 HTTP 400: {"error":{"message":"Invalid size"}}`:             Rejected,
		`context deadline exceeded (Client.Timeout exceeded)`:             Timeout,
		`dial tcp 1.2.3.4:443: connect: connection refused`:               Network,
		`上游返回成功但未解析出产物`:                                                   NoOutput,
		`something odd: {"secret":"x"}`:                                   Generic,
		``:                                                                Generic,
	}
	for raw, want := range cases {
		if got := FromUpstream(raw); got != want {
			t.Errorf("%q → %q，期望 %q", raw, got, want)
		}
		if raw != "" && FromUpstream(raw) == raw {
			t.Errorf("不能原样返回：%q", raw)
		}
	}
}

func TestWrapHidesRawKeepsChain(t *testing.T) {
	if Wrap(nil) != nil {
		t.Fatal("nil 应原样返回")
	}
	raw := fmt.Errorf("上游返回 HTTP 500: {\"error\":\"internal at 10.0.0.3\"}: %w", context.Canceled)
	err := Wrap(raw)
	if err.Error() != Unavailable {
		t.Fatalf("文案 = %q", err.Error())
	}
	if strings.Contains(err.Error(), "10.0.0.3") {
		t.Fatal("原话泄漏")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("错误链要保留")
	}
	var ue *Error
	if !errors.As(err, &ue) || !strings.Contains(ue.Detail(), "10.0.0.3") {
		t.Fatal("Detail 要给原话")
	}
	if Wrap(err) != err {
		t.Fatal("已包过的不再包")
	}
}
