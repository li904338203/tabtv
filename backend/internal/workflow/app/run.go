package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/workflow/domain"
	"github.com/li904338203/tabtv/backend/internal/workflow/repo"
)

// 整图运行（P08-02）：持久化、依赖就绪、分支并发、执行快照、每节点幂等。

// NodeGenRequest 是一个节点的生成受理请求。
type NodeGenRequest struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ProjectID      int64
	NodeID         string
	OperationCode  string
	ModelCode      string
	Params         json.RawMessage
	DeclaredInputs json.RawMessage
	DurationSec    int
	Count          int
	// IdempotencyKey 由 **运行 ID + 节点 ID** 拼成。
	//
	// ⚠️ 这是"重复消息与进程重启不重复扣费"的落点（NEW-05）：
	// 同一次运行里同一个节点无论被派发多少次，受理层都会命中同一条记录
	// 而不是新建一个任务。带上时间戳或随机数就全毁了。
	IdempotencyKey string
	// InputFingerprint 是计划器算出的"这次用的设置"。
	//
	// ⚠️ 受理时**原样存下来**，而不是事后从快照反算：两侧的表示形态不同，
	// 反算出来的值与这里的永远对不上，复用永远不成立（见迁移 00018）。
	InputFingerprint string
}

// NodeGenerator 由生成域实现：受理一个节点的生成。
//
// 返回**实际扣的积分**，运行的总费用按节点逐条累加（NEW-06 的逐项对账）。
type NodeGenerator interface {
	Generate(ctx context.Context, in NodeGenRequest) (generationID int64, credits int, err error)
}

// Runs 由本域的仓储实现。
type Runs interface {
	InsertRun(ctx context.Context, run repo.Run, nodes []repo.NodeRun) error
	GetRun(ctx context.Context, workspaceID, id int64) (repo.Run, error)
	GetRunByID(ctx context.Context, id int64) (repo.Run, error)
	ListRuns(ctx context.Context, workspaceID, projectID int64, limit int) ([]repo.Run, error)
	ListNodes(ctx context.Context, runID int64) ([]repo.NodeRun, error)
	ClaimNode(ctx context.Context, runID int64, nodeID string) (bool, int, error)
	AttachGeneration(ctx context.Context, runID int64, nodeID string, genID int64, credits int) error
	FinishNode(ctx context.Context, runID int64, nodeID, status, errCode, errMsg string) (bool, error)
	MarkNodes(ctx context.Context, runID int64, nodeIDs []string, status, msg string) error
	NodeByGeneration(ctx context.Context, generationID int64) (repo.NodeRun, error)
	FinishRun(ctx context.Context, runID int64, status string, totalCredits int) error
	StaleRuns(ctx context.Context, limit int) ([]repo.Run, error)
	ResetNodes(ctx context.Context, runID int64, nodeIDs []string) (int, error)
	ReopenRun(ctx context.Context, runID int64) error
	CancelRun(ctx context.Context, runID int64, reason string) error
	MarkSkipped(ctx context.Context, runID int64, nodeID string) (bool, error)
	ActiveGenerations(ctx context.Context, runID int64) ([]int64, error)
}

func (s *Service) SetRuns(r Runs)             { s.runs = r }
func (s *Service) SetNodeGen(g NodeGenerator) { s.nodeGen = g }
func (s *Service) SetNextID(f func() int64)   { s.nextID = f }

// ---------------------------------------------------------------- 启动

// StartInput 是一次整图执行请求。
type StartInput struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ProjectID      int64
	MemberLevelID  int64
	TargetNodeIDs  []string
	Force          bool
}

// RunView 是运行的对外视图。
type RunView struct {
	Run   repo.Run
	Nodes []repo.NodeRun
}

// Start 建立并启动一次整图运行（NEW-03、NEW-04）。
//
// 顺序是固定的：**先算计划 → 有问题就退出 → 固定快照 → 一个事务写运行与节点
// → 事务外派发**。
//
//   - 有 issue 时直接退出：一个 run 都不建、一分钱都不扣（NEW-01）。
//   - 快照在**建运行时**固定：此后用户改画布不影响本次运行（NEW-03）。
//     不固定的话，跑到一半改了图，后半段按新图跑、前半段按旧图跑 ——
//     产出对不上，事后也复现不了。
//   - 派发在**事务外**：受理要调目录域算价、要写自己的表，
//     放进事务里等于把一个远程调用塞进事务（08 §5 明令禁止）。
func (s *Service) Start(ctx context.Context, in StartInput) (RunView, error) {
	if s.runs == nil || s.nodeGen == nil || s.nextID == nil {
		return RunView{}, httpx.Errf(errcode.Internal, "运行链路未接入")
	}
	plan, err := s.Plan(ctx, PlanInput{
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID,
		MemberLevelID: in.MemberLevelID, TargetNodeIDs: in.TargetNodeIDs,
		Force: in.Force,
	})
	if err != nil {
		return RunView{}, err
	}
	if len(plan.Issues) > 0 {
		// ⚠️ 把问题原样带回去，而不是一句"图有问题"：用户要知道是哪几个节点。
		return RunView{}, httpx.Errf(errcode.InvalidArgument, issueText(plan.Issues))
	}
	if !plan.Executable() {
		return RunView{}, httpx.Errf(errcode.InvalidArgument,
			"没有需要执行的节点："+plan.Summary())
	}

	snapshot, err := json.Marshal(plan)
	if err != nil {
		return RunView{}, httpx.Wrap(errcode.Internal, err)
	}
	targets, err := json.Marshal(orEmptyIDs(in.TargetNodeIDs))
	if err != nil {
		return RunView{}, httpx.Wrap(errcode.Internal, err)
	}

	run := repo.Run{
		ID: s.nextID(), WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID,
		PayerAccountID: in.PayerAccountID, CreatedByUserID: in.UserID,
		GraphSnapshot: snapshot, TargetNodeIDs: targets,
		Status: string(domain.RunRunning),
	}
	nodes := make([]repo.NodeRun, 0, len(plan.Nodes))
	for _, pn := range plan.Nodes {
		deps, err := json.Marshal(orEmptyIDs(pn.DependsOn))
		if err != nil {
			return RunView{}, httpx.Wrap(errcode.Internal, err)
		}
		nodes = append(nodes, repo.NodeRun{
			ID: s.nextID(), RunID: run.ID, NodeID: pn.ID, DependsOn: deps,
			Status:   string(domain.ActionToNodeStatus(pn.Action)),
			IsReused: pn.Action == domain.ActionReuse,
			// ⚠️ 复用的节点也要挂上那次产出（子任务引用）：
			// 不挂的话下游找不到"上游的产出在哪"，一律受理失败。
			GenerationID: pn.ReuseGenerationID,
		})
	}
	if err := s.runs.InsertRun(ctx, run, nodes); err != nil {
		return RunView{}, httpx.Wrap(errcode.Internal, err)
	}

	// 立刻派发第一批，用户点"运行"就能看到任务动起来。
	s.dispatch(ctx, run, plan)
	return s.Run(ctx, in.WorkspaceID, run.ID)
}

// ---------------------------------------------------------------- 派发

// dispatch 把当前就绪的节点提交出去，并重算运行状态。
//
// ⚠️ 每次都**重新读库**算就绪：内存里那份在并发回写下立刻就旧了。
// 按旧数据派发的结果是漏派（节点永远 pending）或重派（重复扣费）。
//
// ⚠️ 它**只在 API 进程里跑**：派发要走统一受理入口（算价、校验能力、
// 扣费、写 outbox），那是 API 的事务入口，不是 Worker 的活（08 §2）。
// Worker 那侧只推进节点状态，下一批由 API 的扫描循环接手 ——
// 节点动辄几十秒，几秒的派发延迟无关紧要，而把受理搬进 Worker
// 等于在第二个地方重新实现一遍能力矩阵、会员门槛与并发上限。
func (s *Service) dispatch(ctx context.Context, run repo.Run, plan domain.Plan) {
	rows, err := s.runs.ListNodes(ctx, run.ID)
	if err != nil {
		s.warn(ctx, "读取运行节点失败", "runId", run.ID, "error", err)
		return
	}
	// 先把因为上游失败而再也跑不了的标掉 —— 不标的话它们永远 pending，
	// 整次运行既不结束也不推进（NEW-06）。
	if blocked := domain.BlockedByFailure(toRunNodes(rows)); len(blocked) > 0 {
		if err := s.runs.MarkNodes(ctx, run.ID, blocked,
			string(domain.NodeBlocked), "上游没有产出"); err != nil {
			s.warn(ctx, "标记阻塞节点失败", "runId", run.ID, "error", err)
		}
		if rows, err = s.runs.ListNodes(ctx, run.ID); err != nil {
			s.warn(ctx, "读取运行节点失败", "runId", run.ID, "error", err)
			return
		}
	}

	byID := map[string]domain.PlanNode{}
	for _, pn := range plan.Nodes {
		byID[pn.ID] = pn
	}
	for _, nodeID := range domain.ReadyNodes(toRunNodes(rows)) {
		// ⚠️ 先 CAS 抢占再提交：两个调度动作同时看到同一个节点就绪时，
		// 只有一个能抢到。没有它就是同一个节点被提交两次、两份钱。
		claimed, attempt, err := s.runs.ClaimNode(ctx, run.ID, nodeID)
		if err != nil {
			s.warn(ctx, "抢占节点失败", "runId", run.ID, "nodeId", nodeID, "error", err)
			continue
		}
		if !claimed {
			continue
		}
		s.submitNode(ctx, run, byID[nodeID], attempt)
	}
	s.settleRun(ctx, run.ID)
}

// submitNode 提交一个节点的生成任务。
func (s *Service) submitNode(ctx context.Context, run repo.Run, pn domain.PlanNode,
	attempt int) {
	if pn.ID == "" {
		// 计划里找不到这个节点：快照与库不一致，按失败处理而不是静默跳过 ——
		// 静默跳过会让运行永远停在 running。
		return
	}
	inputs, params := s.nodeInputs(ctx, run, pn.ID)
	inputs = s.resolveInputs(ctx, run, pn.ID, inputs)
	genID, credits, err := s.nodeGen.Generate(ctx, NodeGenRequest{
		WorkspaceID: run.WorkspaceID, PayerAccountID: run.PayerAccountID,
		UserID: run.CreatedByUserID, ProjectID: run.ProjectID, NodeID: pn.ID,
		OperationCode: pn.Operation, ModelCode: pn.ModelCode,
		Params: params, DeclaredInputs: inputs, Count: 1,
		// ⚠️ 幂等键 = 运行 + 节点。重复消息与进程重启因此落到
		// generation 已有的幂等上，不重复扣费（NEW-05）。
		// ⚠️ 幂等键带上 attempt：不带的话重试会命中上一次那条失败的
		// generation（受理层认为是重复提交），用户点多少次"重试"
		// 都是同一个失败结果 —— 他再也跑不动这个节点了。
		IdempotencyKey:   fmt.Sprintf("wf:%d:%s:%d", run.ID, pn.ID, attempt),
		InputFingerprint: pn.Fingerprint,
	})
	if err != nil {
		// ⚠️ 受理失败要**落到节点上**，不是只记一条日志：
		// 不落的话节点停在 running，运行永远不结束。
		if _, e := s.runs.FinishNode(ctx, run.ID, pn.ID,
			string(domain.NodeFailed), "accept_failed", errText(err)); e != nil {
			s.warn(ctx, "标记节点失败出错", "runId", run.ID, "nodeId", pn.ID, "error", e)
		}
		return
	}
	if err := s.runs.AttachGeneration(ctx, run.ID, pn.ID, genID, credits); err != nil {
		s.warn(ctx, "挂载生成任务失败", "runId", run.ID, "nodeId", pn.ID, "error", err)
	}
}

// nodeInputs 从**运行快照**里取这个节点的输入与参数。
//
// ⚠️ 取的是快照不是当前画布（NEW-03）：用户在运行期间改了画布，
// 后半段按新图跑、前半段按旧图跑 —— 产出对不上，事后也复现不了。
func (s *Service) nodeInputs(ctx context.Context, run repo.Run, nodeID string) (
	inputs, params json.RawMessage) {
	var snap struct {
		Nodes []struct {
			ID     string          `json:"ID"`
			Inputs json.RawMessage `json:"Inputs"`
			Params json.RawMessage `json:"Params"`
		} `json:"Nodes"`
	}
	if err := json.Unmarshal(run.GraphSnapshot, &snap); err != nil {
		s.warn(ctx, "运行快照解析失败", "runId", run.ID, "error", err)
		return json.RawMessage("[]"), json.RawMessage("{}")
	}
	for _, n := range snap.Nodes {
		if n.ID != nodeID {
			continue
		}
		if len(n.Inputs) > 0 {
			inputs = n.Inputs
		}
		if len(n.Params) > 0 {
			params = n.Params
		}
	}
	if len(inputs) == 0 {
		inputs = json.RawMessage("[]")
	}
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	return inputs, params
}

// resolveInputs 把快照里的"上游节点 ID"换成受理侧认得的引用。
//
// ⚠️ 快照里存的是**节点 ID**（它在建运行时就固定），而受理侧要的是
// **产出 ID**（它要到上游真正跑完才有）。不做这一步转换的话，
// 每个有上游的节点都会收到「引用的产出不存在」—— 整张图只有第一层跑得动。
//
// 三种上游各有各的换法：
//
//   - 跑过的 / 复用的 → 换成它的 `generationId`；
//   - 静态文本节点 → 直接把那段词作为 text 输入（它没有产出）；
//   - 素材节点 → 换成素材码（只贡献提示词，不算媒体）；
//   - 静态素材节点（导演台截图）→ 换成 `{kind:"reference", referenceId}`。
func (s *Service) resolveInputs(ctx context.Context, run repo.Run, nodeID string,
	inputs json.RawMessage) json.RawMessage {
	var refs []map[string]any
	if err := json.Unmarshal(inputs, &refs); err != nil || len(refs) == 0 {
		return inputs
	}
	rows, err := s.runs.ListNodes(ctx, run.ID)
	if err != nil {
		s.warn(ctx, "解析输入时读取节点失败", "runId", run.ID, "error", err)
		return inputs
	}
	genByNode := map[string]int64{}
	for _, n := range rows {
		if n.GenerationID != 0 {
			genByNode[n.NodeID] = n.GenerationID
		}
	}

	var snap domain.Plan
	if err := json.Unmarshal(run.GraphSnapshot, &snap); err != nil {
		return inputs
	}
	planByID := map[string]domain.PlanNode{}
	for _, pn := range snap.Nodes {
		planByID[pn.ID] = pn
	}

	out := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		src, _ := ref["sourceNodeId"].(string)
		if src == "" {
			out = append(out, ref)
			continue
		}
		up := planByID[src]
		switch {
		case domain.IsMaterial(up.Kind):
			code := paramOf(up.Params, "materialCode")
			if code == "" {
				// ⚠️ 用户放了风格节点却没选风格：**丢掉这条输入**而不是
				// 发一个空码下去。空码在受理侧查不到任何素材词，
				// 却会让整个节点受理失败 —— 一个"还没选"的可选项
				// 不该把整张图卡住。
				continue
			}
			ref["kind"] = "material"
			delete(ref, "sourceNodeId")
			ref["materialCode"] = code
			ref["materialType"] = string(up.Kind)
		case up.Kind == domain.KindText && up.ModelCode == "":
			// 静态文本：它没有产出，把词本身送下去。
			ref["kind"] = "text"
			delete(ref, "sourceNodeId")
			ref["text"] = paramOf(up.Params, "prompt")
		case up.StaticMediaID != 0:
			// 静态素材（导演台截图 / 发布节点，DRD-19）：它没有任务，下游直接引用那个媒体。
			// 媒体 ID 取**运行快照**里的，不重读画布：运行中途重新截图不影响这一次运行。
			// sourceNodeId 保留：受理侧报错时据此说出是哪个节点的输入。
			ref["kind"] = "reference"
			ref["referenceId"] = itoa(up.StaticMediaID)
		default:
			if id, ok := genByNode[src]; ok {
				ref["generationId"] = itoa(id)
			}
		}
		out = append(out, ref)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return inputs
	}
	return raw
}

func paramOf(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// ---------------------------------------------------------------- 推进

// AdvanceByGeneration 在某个生成任务到终态后推进运行。
//
// ⚠️ 它**只改状态、不派发**：这段代码跑在 Worker 里，而派发要走 API 的
// 受理入口（见 dispatch 的注释）。下一批由 API 的扫描循环接手。
//
// ⚠️ 与组子项回写同一套约束：跨域被调用，**绝不能牵连主流程** ——
// 出错只告警。为一次状态同步把已完成的任务重跑一遍会再扣一次费。
func (s *Service) AdvanceByGeneration(ctx context.Context, generationID int64,
	status, errCode, errMsg string) error {
	if s.runs == nil {
		return nil
	}
	node, err := s.runs.NodeByGeneration(ctx, generationID)
	if err != nil {
		if errors.Is(err, repo.ErrNodeNotFd) {
			return nil // 不是整图运行里的任务。
		}
		return err
	}
	nodeStatus := string(domain.NodeFailed)
	switch status {
	case "success":
		nodeStatus = string(domain.NodeSuccess)
	case "cancelled":
		nodeStatus = string(domain.NodeCancelled)
	}
	if _, err := s.runs.FinishNode(ctx, node.RunID, node.NodeID,
		nodeStatus, errCode, errMsg); err != nil {
		return err
	}

	// 一个节点失败 → 它的下游再也跑不了，要传递地标掉（NEW-06）。
	// 不标的话它们永远 pending，整次运行既不结束也不推进。
	rows, err := s.runs.ListNodes(ctx, node.RunID)
	if err != nil {
		return err
	}
	if blocked := domain.BlockedByFailure(toRunNodes(rows)); len(blocked) > 0 {
		if err := s.runs.MarkNodes(ctx, node.RunID, blocked,
			string(domain.NodeBlocked), "上游没有产出"); err != nil {
			s.warn(ctx, "标记阻塞节点失败", "runId", node.RunID, "error", err)
		}
	}
	s.settleRun(ctx, node.RunID)
	return nil
}

// settleRun 重算运行状态并在终态时结算总费用。
func (s *Service) settleRun(ctx context.Context, runID int64) {
	rows, err := s.runs.ListNodes(ctx, runID)
	if err != nil {
		s.warn(ctx, "读取运行节点失败", "runId", runID, "error", err)
		return
	}
	status := domain.AggregateRun(toRunNodes(rows))
	total := 0
	for _, n := range rows {
		// ⚠️ 复用的节点**不计费**：它本来就没跑（NEW-05）。
		if !n.IsReused {
			total += n.CreditsCost
		}
	}
	if err := s.runs.FinishRun(ctx, runID, string(status), total); err != nil {
		s.warn(ctx, "写入运行状态失败", "runId", runID, "error", err)
	}
}

// Tick 扫描还在跑的运行并派发新就绪的节点。
//
// 它同时担两件事，这不是巧合 —— 两件事要的是同一个动作：
//
//   - **接力派发**：Worker 那侧只推进节点状态，下一批由这里接手；
//   - **重启恢复**（NEW-05）：进程重启后内存里什么都没有，
//     唯一可靠的事实是库里的节点状态。
//
// ⚠️ 靠的是**重新算就绪**，不是"记得上次派发到哪了"。
// 已经在 running 的节点不会被重复派发（ReadyNodes 只看 pending），
// 抢占又是 CAS 的，所以多个 API 实例同时扫也不会重复提交。
func (s *Service) Tick(ctx context.Context, limit int) (int, error) {
	if s.runs == nil {
		return 0, nil
	}
	runs, err := s.runs.StaleRuns(ctx, limit)
	if err != nil {
		return 0, err
	}
	for _, run := range runs {
		var plan domain.Plan
		if err := json.Unmarshal(run.GraphSnapshot, &plan); err != nil {
			s.warn(ctx, "运行快照解析失败，跳过", "runId", run.ID, "error", err)
			continue
		}
		s.dispatch(ctx, run, plan)
	}
	return len(runs), nil
}

// ---------------------------------------------------------------- 查询

// Run 取一次运行及其节点。
func (s *Service) Run(ctx context.Context, workspaceID, runID int64) (RunView, error) {
	run, err := s.runs.GetRun(ctx, workspaceID, runID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return RunView{}, httpx.Errf(errcode.NotFound, "运行记录不存在")
		}
		return RunView{}, httpx.Wrap(errcode.Internal, err)
	}
	nodes, err := s.runs.ListNodes(ctx, run.ID)
	if err != nil {
		return RunView{}, httpx.Wrap(errcode.Internal, err)
	}
	return RunView{Run: run, Nodes: nodes}, nil
}

// History 取项目的运行历史。
func (s *Service) History(ctx context.Context, workspaceID, projectID, limit int64) (
	[]repo.Run, error) {
	out, err := s.runs.ListRuns(ctx, workspaceID, projectID, int(limit))
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return out, nil
}

// ---------------------------------------------------------------- 工具

func toRunNodes(rows []repo.NodeRun) []domain.RunNode {
	out := make([]domain.RunNode, 0, len(rows))
	for _, n := range rows {
		var deps []string
		if len(n.DependsOn) > 0 {
			_ = json.Unmarshal(n.DependsOn, &deps)
		}
		out = append(out, domain.RunNode{
			NodeID: n.NodeID, DependsOn: deps, Status: domain.NodeStatus(n.Status),
		})
	}
	return out
}

func issueText(issues []domain.Issue) string {
	out := ""
	for i, is := range issues {
		if i > 0 {
			out += "；"
		}
		out += is.Message
	}
	return out
}

func orEmptyIDs(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// ---------------------------------------------------------------- 重试 / 跳过 / 取消

// RetryNode 重试一个失败的节点及其**必要**下游（NEW-05）。
//
// ⚠️ "必要下游"不是"全部下游"：已经成功的后代不重跑 —— 它们的产出还在，
// 重跑要再花一次钱，而用户点的是"重试这个失败的"。
//
// ⚠️ 被它卡住的后代（blocked）要**一起放开**：只把失败那个放回 pending，
// 它跑完之后下游还停在 blocked（终态），永远不会被调度。
func (s *Service) RetryNode(ctx context.Context, workspaceID, runID int64,
	nodeID string) (RunView, error) {
	run, nodes, err := s.loadRun(ctx, workspaceID, runID)
	if err != nil {
		return RunView{}, err
	}
	scope, err := domain.PlanRetry(toRunNodes(nodes), nodeID)
	if err != nil {
		return RunView{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if _, err := s.runs.ResetNodes(ctx, runID, scope.Reset); err != nil {
		return RunView{}, httpx.Wrap(errcode.Internal, err)
	}
	// ⚠️ 运行本身也要放回 running：不放的话恢复扫描捞不到它
	// （只捞 pending/running），那几个 pending 节点永远等不到派发。
	if err := s.runs.ReopenRun(ctx, runID); err != nil {
		return RunView{}, httpx.Wrap(errcode.Internal, err)
	}

	plan, err := s.snapshotPlan(ctx, run)
	if err != nil {
		return RunView{}, err
	}
	s.dispatch(ctx, run, plan)
	return s.Run(ctx, workspaceID, runID)
}

// SkipNode 跳过一个节点，并把它的下游标成 blocked（NEW-06）。
//
// ⚠️ 用户主动跳过与计划器的 skip **不是一回事**：计划器跳过的是
// 素材节点、静态文本与可复用项（它们有产出或不需要产出，下游照常跑）；
// 用户跳过的是"本来会产出而现在没有"，所以下游拿不到输入，必须 blocked。
//
// 混为一谈的话，用户跳过一个图片节点，下游视频节点照样被派发 ——
// 拿不到首帧，跑出来的东西完全不是他要的，而钱已经花了。
func (s *Service) SkipNode(ctx context.Context, workspaceID, runID int64,
	nodeID string) (RunView, error) {
	run, nodes, err := s.loadRun(ctx, workspaceID, runID)
	if err != nil {
		return RunView{}, err
	}
	scope, err := domain.PlanSkip(toRunNodes(nodes), nodeID)
	if err != nil {
		return RunView{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	ok, err := s.runs.MarkSkipped(ctx, runID, nodeID)
	if err != nil {
		return RunView{}, httpx.Wrap(errcode.Internal, err)
	}
	if !ok {
		return RunView{}, httpx.Errf(errcode.InvalidArgument, "节点状态已变，请刷新后重试")
	}
	if err := s.runs.MarkNodes(ctx, runID, scope.Reset,
		string(domain.NodeBlocked), "上游被跳过，拿不到输入"); err != nil {
		return RunView{}, httpx.Wrap(errcode.Internal, err)
	}

	plan, err := s.snapshotPlan(ctx, run)
	if err != nil {
		return RunView{}, err
	}
	s.dispatch(ctx, run, plan)
	return s.Run(ctx, workspaceID, runID)
}

// Cancel 取消整次运行（NEW-06）。
//
// ⚠️ 顺序是**先标记运行、再取消在跑的任务** —— 这一条是联调改过来的。
//
// 原本写的是反过来（先取消任务再标记运行），理由是"标记与取消之间
// 新派发的节点会漏网"。实测证明那个理由不成立、而代价很实在：
//
//   - 不成立：调度只发生在 `Tick`，而它只捞 pending/running 的运行 ——
//     运行一旦标成 cancelled，就不会再有新节点被派发。
//   - 代价：取消子任务会**同步触发回写**（AdvanceByGeneration），
//     回写把节点标成 cancelled、把下游标 blocked、然后**结算整次运行**。
//     等轮到 `CancelRun` 时运行已经是终态，它的 where 条件不再匹配 ——
//     于是用户按了取消，界面上却显示"运行失败"。
//
// ⚠️ 单个任务取消失败**不中断其余的**：它可能刚好跑完了，
// 那就让回写去决定终态。已经跑完的取消不了 —— 那时钱已经花在上游了，
// 假装取消成功只会让用户以为不用付费。
func (s *Service) Cancel(ctx context.Context, workspaceID, runID int64,
	reason string) (RunView, error) {
	if _, _, err := s.loadRun(ctx, workspaceID, runID); err != nil {
		return RunView{}, err
	}
	// 先把在跑的任务列出来 —— 下一步就要把节点标成 cancelled 了。
	var active []int64
	if s.canceller != nil {
		var err error
		if active, err = s.runs.ActiveGenerations(ctx, runID); err != nil {
			return RunView{}, httpx.Wrap(errcode.Internal, err)
		}
	}
	if err := s.runs.CancelRun(ctx, runID, reason); err != nil {
		return RunView{}, httpx.Wrap(errcode.Internal, err)
	}
	for _, id := range active {
		if err := s.canceller.Cancel(ctx, workspaceID, id, reason); err != nil {
			s.warn(ctx, "取消子任务失败", "runId", runID, "generationId", id, "error", err)
		}
	}
	return s.Run(ctx, workspaceID, runID)
}

// GenerationCanceller 由生成域实现：取消一个子任务（连带退款）。
type GenerationCanceller interface {
	Cancel(ctx context.Context, workspaceID, generationID int64, reason string) error
}

func (s *Service) SetCanceller(c GenerationCanceller) { s.canceller = c }

// loadRun 取运行与节点（带归属校验）。
func (s *Service) loadRun(ctx context.Context, workspaceID, runID int64) (
	repo.Run, []repo.NodeRun, error) {
	if s.runs == nil {
		return repo.Run{}, nil, httpx.Errf(errcode.Internal, "运行链路未接入")
	}
	run, err := s.runs.GetRun(ctx, workspaceID, runID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return repo.Run{}, nil, httpx.Errf(errcode.NotFound, "运行记录不存在")
		}
		return repo.Run{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	nodes, err := s.runs.ListNodes(ctx, run.ID)
	if err != nil {
		return repo.Run{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	return run, nodes, nil
}

func (s *Service) snapshotPlan(ctx context.Context, run repo.Run) (domain.Plan, error) {
	var plan domain.Plan
	if err := json.Unmarshal(run.GraphSnapshot, &plan); err != nil {
		s.warn(ctx, "运行快照解析失败", "runId", run.ID, "error", err)
		return domain.Plan{}, httpx.Errf(errcode.Internal, "运行快照已损坏，无法继续")
	}
	return plan, nil
}

// Cost 取一次运行的费用对账（NEW-06）。
func (s *Service) Cost(ctx context.Context, workspaceID, runID int64) (
	domain.RunCost, error) {
	_, nodes, err := s.loadRun(ctx, workspaceID, runID)
	if err != nil {
		return domain.RunCost{}, err
	}
	out := make([]domain.CostNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, domain.CostNode{
			NodeID: n.NodeID, Status: domain.NodeStatus(n.Status),
			CreditsCost: n.CreditsCost, IsReused: n.IsReused,
			ErrorCode: n.ErrorCode, ErrorMessage: n.ErrorMessage,
		})
	}
	return domain.AggregateCost(out), nil
}
