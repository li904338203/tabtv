package ffsem

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 同时持有的名额永远不超过容量。
func TestAcquire_不超过容量(t *testing.T) {
	s := New(2)
	var cur, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.Acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			cur.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if p := peak.Load(); p < 1 || p > 2 {
		t.Fatalf("同时持有的名额应在 1~2，实际峰值 %d", p)
	}
	if s.InUse() != 0 {
		t.Fatalf("全部释放后应为 0：%d", s.InUse())
	}
}

// 等名额时被取消：立即返回 ctx 的错误，不占名额。
func TestAcquire_取消时放弃(t *testing.T) {
	s := New(1)
	release, _ := s.Acquire(context.Background())
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r2, err := s.Acquire(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回超时：%v", err)
	}
	r2() // 失败时返回的 release 也能安全调用
	if s.InUse() != 1 {
		t.Fatalf("被取消的那次不该占名额：%d", s.InUse())
	}
}

// release 多调只放一次：放两次会凭空多出一个名额。
func TestRelease_多调只放一次(t *testing.T) {
	s := New(1)
	r1, _ := s.Acquire(context.Background())
	r1()
	r1()
	r2, _ := s.Acquire(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(ctx); err == nil {
		t.Fatal("容量为 1、已被占用时第二次不该拿到")
	}
	r2()
}

func TestNil与下限(t *testing.T) {
	var s *Sem
	r, err := s.Acquire(context.Background())
	if err != nil || s.Cap() != 0 || s.InUse() != 0 {
		t.Fatalf("nil 表示不限：%v", err)
	}
	r()
	if New(0).Cap() != 1 || New(-3).Cap() != 1 {
		t.Fatal("容量下限为 1")
	}
}

// 绊线：只依赖标准库（叶子包）。
func TestOnlyStdlib(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Skipf("go list 不可用：%v", err)
	}
	for _, line := range strings.Fields(string(out)) {
		if line != "github.com/li904338203/tabtv/backend/internal/platform/ffsem" {
			t.Fatalf("ffsem 只能依赖标准库，出现了 %s", line)
		}
	}
}
