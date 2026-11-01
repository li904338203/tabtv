package app

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubChecker struct {
	kind string
	ids  []int64
	err  error
}

func (c stubChecker) Kind() string { return c.kind }
func (c stubChecker) References(context.Context, int64) ([]int64, error) {
	return c.ids, c.err
}

// 查询失败按「有引用」处理（MKT-T-23 验收）：删不掉，而不是误删。
//
// repo 给 nil：查引用失败时必须在碰仓储之前返回，碰了就 panic —— 那本身就是失败。
func TestDelete_引用查询失败时拒绝删除(t *testing.T) {
	s := &Service{}
	s.AddReferenceChecker(stubChecker{kind: UsageAgentSession, ids: nil})
	s.AddReferenceChecker(stubChecker{kind: UsageAgentMarketing, err: errors.New("db down")})
	if err := s.Delete(context.Background(), 1, 2); err == nil {
		t.Fatal("引用查询失败时应拒绝删除")
	}
}

func TestDescribeReferences_Agent两类文案(t *testing.T) {
	got := describeReferences([]Reference{
		{Kind: UsageAgentMarketing, RefID: 1},
		{Kind: UsageAgentMarketing, RefID: 2},
		{Kind: UsageAgentSession, RefID: 3},
	})
	if got != "营销会话、Agent 会话" {
		t.Fatalf("文案应分别点名营销会话与 Agent 会话且同类只说一次，got=%q", got)
	}
	if strings.Contains(got, "agent_session") {
		t.Fatalf("不该把内部 Kind 原样给用户：%q", got)
	}
}
