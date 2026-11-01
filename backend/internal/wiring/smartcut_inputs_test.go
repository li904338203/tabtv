package wiring

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
)

// 素材条与前端 `utils/smartcutInputs.ts` 读**同一批** fixture（SCT-25，SCR-11）：
// 画布 → canvas/domain.InboundSources → SmartcutSources → smartcut/domain.ResolveInputs，走的就是服务端那条路。
// 期望值在 fixture 里手算写死，不拿任一侧的实现去算。
const inputsFixtureDir = "../../../api/fixtures/smartcut/inputs"

type inputsFixture struct {
	Why    string          `json:"why"`
	NodeID string          `json:"nodeId"`
	Canvas json.RawMessage `json:"canvas"`
	Lookup struct {
		Generations map[string]struct {
			Status    string           `json:"status"`
			Operation string           `json:"operation"`
			Reason    string           `json:"reason"`
			Outputs   *[]fixtureOutput `json:"outputs"`
		} `json:"generations"`
		Media map[string]fixtureOutput `json:"media"`
	} `json:"lookup"`
	Clips []struct {
		ID           string `json:"id"`
		SourceNodeID string `json:"sourceNodeId"`
		MediaID      string `json:"mediaId"`
	} `json:"clips"`
	Expect struct {
		Items        json.RawMessage `json:"items"`
		Omitted      int             `json:"omitted"`
		Disconnected []string        `json:"disconnected"`
		UnusedCount  int             `json:"unusedCount"`
	} `json:"expect"`
}

type fixtureOutput struct {
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	MediaID       string `json:"mediaId"`
	DurationMs    *int   `json:"durationMs"`
	HasAudio      *bool  `json:"hasAudio"`
	PosterMediaID string `json:"posterMediaId"`
}

type fixtureLookup struct{ f *inputsFixture }

func (l fixtureLookup) Generation(id string) (smartcutdomain.GenerationFact, bool) {
	g, ok := l.f.Lookup.Generations[id]
	if !ok {
		return smartcutdomain.GenerationFact{}, false
	}
	fact := smartcutdomain.GenerationFact{Status: g.Status, Operation: g.Operation, Reason: g.Reason}
	if g.Outputs != nil {
		fact.OutputsKnown = true
		for _, o := range *g.Outputs {
			fact.Outputs = append(fact.Outputs, smartcutdomain.OutputFact{MediaID: o.MediaID, DurationMs: o.DurationMs,
				HasAudio: o.HasAudio, PosterMediaID: o.PosterMediaID})
		}
	}
	return fact, true
}

func (l fixtureLookup) Media(id string) (smartcutdomain.MediaFact, bool) {
	m, ok := l.f.Lookup.Media[id]
	if !ok {
		return smartcutdomain.MediaFact{}, false
	}
	return smartcutdomain.MediaFact{Status: m.Status, Reason: m.Reason, DurationMs: m.DurationMs, HasAudio: m.HasAudio,
		PosterMediaID: m.PosterMediaID}, true
}

func TestSmartcutInputs_与fixture一致(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(inputsFixtureDir, "*.json"))
	if err != nil || len(files) < 10 {
		t.Fatalf("fixture 不全：%d %v", len(files), err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var f inputsFixture
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			sources, err := SmartcutSources(f.Canvas, f.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			var used []smartcutdomain.UsedClip
			var refs []smartcutdomain.ClipRef
			for _, c := range f.Clips {
				used = append(used, smartcutdomain.UsedClip{SourceNodeID: c.SourceNodeID, MediaID: c.MediaID})
				refs = append(refs, smartcutdomain.ClipRef{ID: c.ID, SourceNodeID: c.SourceNodeID})
			}
			items, omitted := smartcutdomain.ResolveInputs(sources, fixtureLookup{&f}, used)
			got, _ := json.Marshal(items)
			if !sameJSON(t, got, f.Expect.Items) {
				t.Fatalf("%s\n素材条不一致：\n  得到 %s\n  期望 %s", f.Why, got, compact(f.Expect.Items))
			}
			if omitted != f.Expect.Omitted {
				t.Fatalf("omitted %d，应为 %d", omitted, f.Expect.Omitted)
			}
			if d := smartcutdomain.DisconnectedClipIDs(refs, sources); !reflect.DeepEqual(d, f.Expect.Disconnected) {
				t.Fatalf("断开的片段 %v，应为 %v", d, f.Expect.Disconnected)
			}
			if n := smartcutdomain.UnusedCount(items); n != f.Expect.UnusedCount {
				t.Fatalf("未使用 %d，应为 %d", n, f.Expect.UnusedCount)
			}
		})
	}
}

// 坏画布报错，不静默变空：素材条一空，「连线即入轨」会把所有片段当成来源已断开。
func TestSmartcutSources_坏画布报错(t *testing.T) {
	if _, err := SmartcutSources(json.RawMessage(`{"nodes":`), "sc1"); err == nil {
		t.Fatal("坏 JSON 要报错")
	}
	if s, err := SmartcutSources(json.RawMessage(`{"nodes":[],"edges":[]}`), "sc1"); err != nil || len(s) != 0 {
		t.Fatalf("节点不在画布上（还没保存）时为空：%v %v", s, err)
	}
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

func compact(b []byte) string {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return string(b)
	}
	return buf.String()
}
