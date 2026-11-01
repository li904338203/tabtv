package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// ---------------------------------------------------------------- 内存版 Store

type ptStore struct {
	mu       sync.Mutex
	session  repo.Session
	cards    []repo.Card
	skills   map[string]repo.Skill
	refs     map[string]repo.SkillReference // key: skillID/name
	insertEr error
}

func newPtStore() *ptStore {
	return &ptStore{session: repo.Session{ID: 10, SkillCodes: json.RawMessage(`[]`)},
		skills: map[string]repo.Skill{}, refs: map[string]repo.SkillReference{}}
}

func (s *ptStore) GetByID(context.Context, int64) (repo.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session, nil
}

func (s *ptStore) UpdateSkills(_ context.Context, _ int64, codes json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session.SkillCodes = codes
	return nil
}

func (s *ptStore) GetSkillByCode(_ context.Context, code string) (repo.Skill, error) {
	sk, ok := s.skills[code]
	if !ok {
		return repo.Skill{}, repo.ErrNotFound
	}
	return sk, nil
}

func (s *ptStore) SkillReferenceByName(_ context.Context, skillID int64, name string) (repo.SkillReference, error) {
	r, ok := s.refs[fmt.Sprintf("%d/%s", skillID, name)]
	if !ok {
		return repo.SkillReference{}, repo.ErrNotFound
	}
	return r, nil
}

func (s *ptStore) InsertCard(_ context.Context, _ repo.Querier, c repo.Card) (repo.Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertEr != nil {
		return repo.Card{}, s.insertEr
	}
	c.Status = domain.CardPending
	s.cards = append(s.cards, c)
	return c, nil
}

func (s *ptStore) VoidCard(_ context.Context, _ repo.Querier, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cards {
		if s.cards[i].ID == id && s.cards[i].Status == domain.CardPending {
			s.cards[i].Status = domain.CardVoided
			return true, nil
		}
	}
	return false, nil
}

func (s *ptStore) PendingBySession(_ context.Context, sessionID int64) ([]repo.Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []repo.Card
	for _, c := range s.cards {
		if c.SessionID == sessionID && c.Status == domain.CardPending {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *ptStore) Pool() repo.Querier { return nil }

// AppendScenePins 模拟库里的规则：同 kind+code 只留第一次。
func (s *ptStore) AppendScenePins(_ context.Context, _ int64, pins json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	si, err := app.ParseSceneInput(s.session.SceneInput)
	if err != nil {
		return err
	}
	var add []app.ScenePinEntry
	if err := json.Unmarshal(pins, &add); err != nil {
		return err
	}
	for _, p := range add {
		dup := false
		for _, e := range si.Pins {
			if e.Kind == p.Kind && e.Code == p.Code {
				dup = true
			}
		}
		if !dup {
			si.Pins = append(si.Pins, p)
		}
	}
	s.session.SceneInput, _ = json.Marshal(si)
	return nil
}

// ---------------------------------------------------------------- 其余端口

type ptCanvas struct {
	data    json.RawMessage
	version int
	applied []app.ApplyOpsInput
}

func (c *ptCanvas) Read(context.Context, int64, int64) (app.CanvasView, error) {
	nodes, edges := app.SummarizeCanvas(c.data)
	return app.CanvasView{Version: c.version, CanvasData: c.data, Nodes: nodes, EdgeCount: edges}, nil
}

func (c *ptCanvas) Apply(_ context.Context, in app.ApplyOpsInput) (app.ApplyOpsResult, error) {
	c.applied = append(c.applied, in)
	return app.ApplyOpsResult{FromVersion: c.version, ToVersion: c.version + 1, NodeIDs: []string{"a_doc1"}}, nil
}

type ptGens map[string]app.NodeGeneration

func (g ptGens) LatestByNode(context.Context, int64, int64) (map[string]app.NodeGeneration, error) {
	return g, nil
}

type ptAssets struct {
	page app.AssetPage
	got  app.AssetQuery
}

func (a *ptAssets) ListAssets(_ context.Context, _ int64, q app.AssetQuery) (app.AssetPage, error) {
	a.got = q
	return a.page, nil
}

// ptPricer：每个模型一个单价，按 Quantity 相乘；blocked 里的模型报不出价。
type ptPricer struct {
	unit    map[string]int
	blocked map[string]string
	// faults：模型码 → 参数错误（ArgFault + 原因），模拟 wiring.quoteBlock 判出的「模型写错了参数」。
	faults map[string]app.PricedItem
	// opFaults：「模型/操作」→ 不可用（MKT-T-28：模型没绑 video2video 等）。
	opFaults map[string]app.PricedItem
	// maxSec：模型码 → 单次最长时长；视频超过它按「时长越界」报不出价（模拟报价侧的协议范围校验）。
	maxSec map[string]int
	calls  []domain.PlanItem
}

func (p *ptPricer) PriceItem(_ context.Context, it domain.PlanItem, _ int64) (app.PricedItem, error) {
	p.calls = append(p.calls, it)
	it.Credits = 0
	f, ok := p.faults[it.ModelCode]
	if !ok {
		f, ok = p.opFaults[it.ModelCode+"/"+it.OperationCode]
	}
	if ok {
		it.BlockedCode, it.BlockedReason = f.BlockedCode, f.BlockedReason
		return app.PricedItem{Item: it, BlockedCode: f.BlockedCode, BlockedReason: f.BlockedReason,
			ArgFault: f.ArgFault, Operation: f.Operation}, nil
	}
	if max := p.maxSec[it.ModelCode]; max > 0 && it.Kind == "video" && it.Quantity > max {
		reason := fmt.Sprintf("视频时长必须在 1 到 %d 秒之间（当前 %d）", max, it.Quantity)
		it.BlockedCode, it.BlockedReason = "INVALID_ARGUMENT", reason
		return app.PricedItem{Item: it, BlockedCode: it.BlockedCode, BlockedReason: reason,
			ArgFault: app.PlanFaultQuantity}, nil
	}
	if reason, ok := p.blocked[it.ModelCode]; ok {
		it.BlockedCode, it.BlockedReason = "MODEL_UNAVAILABLE", reason
		return app.PricedItem{Item: it, BlockedCode: it.BlockedCode, BlockedReason: reason}, nil
	}
	it.Credits = p.unit[it.ModelCode] * it.Quantity
	return app.PricedItem{Item: it, Unit: "per_second", Available: true, Operation: it.OperationCode}, nil
}

type ptBalances int

func (b ptBalances) Balance(context.Context, int64) (int, error) { return int(b), nil }

type ptDocs struct {
	docs map[string]app.Document
	puts []app.PutDocumentInput
}

func (d *ptDocs) Get(_ context.Context, _, _ int64, _ collab.Role, path string) (app.Document, error) {
	doc, ok := d.docs[path]
	if !ok {
		return app.Document{}, app.ErrDocumentNotFound
	}
	return doc, nil
}

func (d *ptDocs) Put(_ context.Context, in app.PutDocumentInput) (app.Document, error) {
	d.puts = append(d.puts, in)
	cur, ok := d.docs[in.Path]
	if in.Version == 0 && ok {
		return app.Document{}, fmt.Errorf("已存在")
	}
	if in.Version != 0 && (!ok || cur.Version != in.Version) {
		return app.Document{}, fmt.Errorf("版本冲突")
	}
	doc := app.Document{ID: 900 + int64(len(d.docs)), Path: in.Path, Content: in.Content, Version: in.Version + 1}
	if ok {
		doc.ID = cur.ID
	}
	d.docs[in.Path] = doc
	return doc, nil
}

// ptRenderer：{{name}} 换成变量；缺变量报错（与 catalog/domain.RenderPrompt 同语义）。
type ptRenderer struct{}

func (ptRenderer) RenderSkill(content string, vars map[string]string) (string, error) {
	out := content
	for {
		i := strings.Index(out, "{{")
		if i < 0 {
			return out, nil
		}
		j := strings.Index(out[i:], "}}")
		if j < 0 {
			return "", fmt.Errorf("占位符未闭合")
		}
		name := out[i+2 : i+j]
		v, ok := vars[name]
		if !ok {
			return "", fmt.Errorf("渲染缺少变量：%s", name)
		}
		out = out[:i] + v + out[i+j+2:]
	}
}

type ptEvents struct{ evs []any }

func (e *ptEvents) Publish(_ context.Context, _ string, p any) error {
	e.evs = append(e.evs, p)
	return nil
}

type ptEnv struct {
	store  *ptStore
	canvas *ptCanvas
	assets *ptAssets
	pricer *ptPricer
	docs   *ptDocs
	events *ptEvents
	deps   PlanningDeps
	nextID int64
}

func newPtEnv() *ptEnv {
	e := &ptEnv{
		store:  newPtStore(),
		canvas: &ptCanvas{data: json.RawMessage(`{"nodes":[],"edges":[]}`), version: 1},
		assets: &ptAssets{},
		pricer: &ptPricer{unit: map[string]int{"seedance": 4, "img": 2}, blocked: map[string]string{}},
		docs:   &ptDocs{docs: map[string]app.Document{}},
		events: &ptEvents{},
		nextID: 1000,
	}
	e.deps = PlanningDeps{Store: e.store, Canvas: e.canvas, Generations: ptGens{}, Assets: e.assets,
		Pricer: e.pricer, Balances: ptBalances(1000), Documents: e.docs, Skills: ptRenderer{},
		Events: e.events, NextID: func() int64 { e.nextID++; return e.nextID },
		Now: func() time.Time { return time.Unix(0, 0) }}
	return e
}

func ptRC(state domain.State) RunContext {
	return RunContext{WorkspaceID: 1, ProjectID: 2, SessionID: 10, TurnID: 20, StepID: 30, Attempt: 1,
		UserID: 3, Role: collab.RoleEditor, PayerAccountID: 4, State: state, ModelCode: "claude"}
}

// tool 按名字取（用 PlanningTools 装出来的那一份，顺带验证装配）。
func (e *ptEnv) tool(name domain.ToolName) Tool {
	for _, t := range PlanningTools(e.deps) {
		if t.Name() == name {
			return t
		}
	}
	panic("未装配工具 " + string(name))
}
