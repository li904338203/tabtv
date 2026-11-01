package domain

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDomain_不依赖数据库与网络：落点、校验、渲染计划都是纯函数，前端照同一套规则写第二份
// 并由共享 fixture 绊住（SC-N-15）；一旦这里能碰库或网络，「同一份输入同一个结果」就不成立了。
func TestDomain_不依赖数据库与网络(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list 不可用：%v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "database/sql", dep == "net", dep == "net/http", strings.Contains(dep, "jackc/pgx"),
			strings.HasPrefix(dep, "github.com/li904338203/tabtv/backend/internal/") && dep != "github.com/li904338203/tabtv/backend/internal/timeline/domain":
			t.Errorf("timeline/domain 依赖了 %s", dep)
		}
	}
}
