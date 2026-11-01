package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 扫描循环里挂着非生成对象的那条路径（AGT-50）：组 / 整图运行 / 渲染 / 一批资产图 / 同步文本调用。
//
// 与第一期 generate 那条路径（ref_type = generation，见 scanner.go 的 E）按 ref_type 互斥：
//   - 唤醒（A）：等待步骤的 generation_id 为空时，按这一步额度条目上的 ref 查终态；
//   - 结算（E'）：条目 charged 且步骤已不在 running → 按**账本**算实扣与退回，写进条目。
//
// ⚠️ 只回写额度账，**不发起任何退款**：失败的金额由各域原有的退款路径退回（组子项 / 整图节点走
// 生成域、渲染走时间线域、同步文本调用走脚本域），这里只等账本上真的有了退款流水再归还额度 ——
// 账还没退就先归还的话，Agent 能在钱没回来时再花一次。
// ⚠️ 部分成功不摊平：10 个子项失败 3 个，条目仍是 charged，refunded 记那 3 个的钱；
// 「已扣 / 退回」与账本 credit_transaction 逐笔一致（RefResult 的两个数只从账本来）。

// refWait 是一个等待步骤所等的业务对象及其当前结局。
type refWait struct {
	// has=false：这一步没有挂业务对象（没有额度条目或条目没有引用）——没有可等的，按成功处理。
	has bool
	ref BudgetRef
	res RefResult
}

func (w refWait) terminal() bool { return !w.has || w.res.Terminal }

func (w refWait) label() string {
	if !w.has {
		return "无关联任务"
	}
	return fmt.Sprintf("%s %d", refTypeLabel(w.ref.Type), w.ref.ID)
}

func refTypeLabel(t string) string {
	switch t {
	case repo.RefScriptGroup:
		return "组"
	case repo.RefWorkflowRun:
		return "整图运行"
	case repo.RefRenderJob:
		return "渲染任务"
	case repo.RefAssetBatch:
		return "资产出图批次"
	case repo.RefScriptLLMCall, repo.RefScriptCompose:
		return "脚本调用"
	}
	return t
}

// refWaits 查一批等待步骤里 generation_id 为空的那些所等的对象（一步一条额度条目）。
// 返回的 map 对每一个这样的步骤都有一项（查不到条目的 has=false）；Refs 未接入时返回空 map，
// 调用方据此不唤醒（宁可多等，也不对着一个不知道结局的对象做总结）。
func (s *Scanner) refWaits(ctx context.Context, sess repo.Session, steps []repo.Step) (map[int64]refWait, error) {
	out := map[int64]refWait{}
	var ids []int64
	for _, st := range steps {
		if st.GenerationID == 0 {
			ids = append(ids, st.ID)
		}
	}
	if len(ids) == 0 || s.d.Refs == nil {
		return out, nil
	}
	entries, err := s.d.Repo.BudgetsBySteps(ctx, s.d.Repo.Pool(), ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		e, ok := entries[id]
		if !ok || e.RefType == "" || e.RefType == repo.RefGeneration {
			out[id] = refWait{}
			continue
		}
		ref := BudgetRef{Type: e.RefType, ID: e.RefID, IDs: e.RefIDs, PayerAccountID: sess.PayerAccountID}
		res, err := s.d.Refs.RefStatus(ctx, sess.WorkspaceID, ref)
		if err != nil {
			return nil, fmt.Errorf("查 %s %d 的状态: %w", e.RefType, e.RefID, err)
		}
		out[id] = refWait{has: true, ref: ref, res: res}
	}
	return out, nil
}

// refStepOutcome 把业务对象的结局换成步骤终态与展示内容。
//
// partial_success 算 success：这一步「做成了」，缺的那几项在 summary 里逐条说清，
// 让模型决定重试哪几项 —— 判成失败的话模型会把已经成功的也重跑一遍（每一项都在花钱）。
func refStepOutcome(step repo.Step, w refWait) (status string, display json.RawMessage, errMsg string) {
	result := map[string]any{}
	switch {
	case !w.has:
		status = repo.StepSuccess
	case !w.res.Found:
		status, errMsg = repo.StepFailed, refTypeLabel(w.ref.Type)+"不存在或已被清理"
	case w.res.Status == "success" || w.res.Status == "partial_success":
		status = repo.StepSuccess
	case w.res.Status == "cancelled":
		status, errMsg = repo.StepCancelled, nonEmpty(w.res.Error, "已取消")
	default:
		status, errMsg = repo.StepFailed, nonEmpty(w.res.Error, nonEmpty(w.res.Summary, "执行失败"))
	}
	if w.has {
		result["refType"] = w.ref.Type
		result["refId"] = idStr(w.ref.ID)
		result["refStatus"] = w.res.Status
		if w.res.Summary != "" {
			result["summary"] = truncRunes(w.res.Summary, 500)
		}
	}
	result["status"] = status
	if errMsg != "" {
		result["error"] = errMsg
	}
	return status, mergeDisplay(step.Display, result), errMsg
}

// scanSettle 是 E 的第二条路径：挂着非生成对象的 charged 条目按账本结算。
func (s *Scanner) scanSettle(ctx context.Context, limit int, st *Stats) error {
	if s.d.Refs == nil {
		return nil
	}
	cands, err := s.d.Repo.UnsettledCandidates(ctx, limit)
	if err != nil {
		return fmt.Errorf("扫描待结算额度: %w", err)
	}
	var errs []error
	for _, c := range cands {
		if err := s.settleRef(ctx, c, st); err != nil {
			errs = append(errs, fmt.Errorf("额度条目 %d: %w", c.Entry.ID, err))
		}
	}
	return errors.Join(errs...)
}

// settleRef 结算一条：对象终态 → 步骤还在 waiting 的补终态；账本上该退的都退了 → 写实扣与退回。
func (s *Scanner) settleRef(ctx context.Context, c repo.SettleCandidate, st *Stats) error {
	e := c.Entry
	ref := BudgetRef{Type: e.RefType, ID: e.RefID, IDs: e.RefIDs, PayerAccountID: c.PayerAccountID}
	res, err := s.d.Refs.RefStatus(ctx, c.WorkspaceID, ref)
	if err != nil {
		return err
	}
	if !res.Terminal {
		return nil
	}
	if c.StepStatus == repo.StepWaiting {
		step, err := s.d.Repo.GetStep(ctx, e.StepID)
		if err != nil {
			return err
		}
		status, display, errMsg := refStepOutcome(step, refWait{has: true, ref: ref, res: res})
		changed, err := s.d.Repo.FinishStep(ctx, s.d.Repo.Pool(), e.StepID, status, display, errMsg)
		if err != nil {
			return err
		}
		if changed {
			st.StepsFinished++
			s.publish(ctx, c.ProjectID, &AgentStepEvent{Type: EventAgentStep, SessionID: idStr(e.SessionID),
				TurnID: idStr(c.TurnID), StepID: idStr(e.StepID), Tool: c.Tool, Status: status, Display: display})
		}
	}
	if !res.Settled {
		return nil // 退款还没到账：等生成域 / 时间线域 / 脚本域的补退款，之后再归还
	}
	ok, err := s.d.Repo.SettleEntry(ctx, e.ID, res.Consumed, res.Refunded, res.LedgerIDs)
	if err != nil {
		return err
	}
	if ok {
		st.BudgetSettled++
		if res.Refunded > 0 {
			st.BudgetRefunded++
		}
		s.d.Logger.Info("额度条目已按账本结算", "sessionId", e.SessionID, "entryId", e.ID,
			"refType", e.RefType, "refId", e.RefID, "quoted", e.Quoted, "consumed", res.Consumed,
			"refunded", res.Refunded)
	}
	return nil
}
