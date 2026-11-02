// Command testpg 管理集成测试用的共享内嵌 PG 集群（TT-T-04）。
//
//	go run ./tools/testpg clean   删除全部测试库（t_*），保留迁移模板与集群
//	go run ./tools/testpg stop    停止集群
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/li904338203/tabtv/backend/internal/platform/pgembed/testcluster"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法：testpg clean|stop")
		os.Exit(2)
	}
	ctx := context.Background()
	switch os.Args[1] {
	case "clean":
		n, err := testcluster.Clean(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if n > 0 {
			fmt.Printf("已删除 %d 个测试库\n", n)
		}
	case "stop":
		if err := testcluster.Stop(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "未知命令：", os.Args[1])
		os.Exit(2)
	}
}
