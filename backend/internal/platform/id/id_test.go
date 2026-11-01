package id

import (
	"sync"
	"testing"
)

func TestNext_单调递增且唯一(t *testing.T) {
	g, err := New(1)
	if err != nil {
		t.Fatal(err)
	}
	const n = 10000
	seen := make(map[int64]struct{}, n)
	var prev int64
	for i := 0; i < n; i++ {
		v := g.Next()
		if v <= prev {
			t.Fatalf("第 %d 个 ID 未递增: %d <= %d", i, v, prev)
		}
		if _, dup := seen[v]; dup {
			t.Fatalf("ID 重复: %d", v)
		}
		seen[v] = struct{}{}
		prev = v
	}
}

// 并发下不得产生重复 ID —— 重复主键会让整条写入链路失败。
func TestNext_并发唯一(t *testing.T) {
	g, _ := New(3)
	const goroutines, perG = 16, 500

	var mu sync.Mutex
	seen := make(map[int64]struct{}, goroutines*perG)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int64, 0, perG)
			for j := 0; j < perG; j++ {
				local = append(local, g.Next())
			}
			mu.Lock()
			defer mu.Unlock()
			for _, v := range local {
				if _, dup := seen[v]; dup {
					t.Errorf("并发产生重复 ID: %d", v)
				}
				seen[v] = struct{}{}
			}
		}()
	}
	wg.Wait()
	if len(seen) != goroutines*perG {
		t.Fatalf("期望 %d 个唯一 ID，实际 %d", goroutines*perG, len(seen))
	}
}

// 不同节点号必须产生互不相同的 ID，否则多实例部署会撞主键。
func TestNext_节点隔离(t *testing.T) {
	a, _ := New(1)
	b, _ := New(2)
	seen := make(map[int64]struct{})
	for i := 0; i < 1000; i++ {
		for _, v := range []int64{a.Next(), b.Next()} {
			if _, dup := seen[v]; dup {
				t.Fatalf("跨节点产生重复 ID: %d", v)
			}
			seen[v] = struct{}{}
		}
	}
}

// ID 必须落在 JS 安全整数之外 —— 这正是对外必须序列化为 string 的原因（GO-34）。
func TestNext_超出JS安全整数(t *testing.T) {
	g, _ := New(0)
	const jsMaxSafe = int64(1)<<53 - 1
	if v := g.Next(); v <= jsMaxSafe {
		t.Fatalf("ID %d 未超出 JS 安全整数范围，雪花位宽配置可能有误", v)
	}
}

func TestNew_节点号越界(t *testing.T) {
	if _, err := New(-1); err == nil {
		t.Error("负数节点号应被拒绝")
	}
	if _, err := New(1024); err == nil {
		t.Error("超出 10 位的节点号应被拒绝")
	}
}
