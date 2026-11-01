package domain

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 分镜解析

func TestParseStoryboard_正常产出(t *testing.T) {
	raw := `{"globalStyle":"水墨","shots":[
	  {"shotNo":5,"description":"@唐僧 走在山路上","durationSec":8,
	   "entities":[{"name":"唐僧","type":"character"}],
	   "startState":"远景起","endState":"近景收"},
	  {"shotNo":9,"description":"山门前","durationSec":3}],
	 "assets":[{"name":"唐僧","type":"character","appearance":"僧袍"}]}`
	sb, err := ParseStoryboard(raw)
	if err != nil {
		t.Fatal(err)
	}
	// ⚠️ 镜号一律重排：模型跳号、重号都见过，而镜号进唯一索引。
	if sb.Shots[0].ShotNo != 1 || sb.Shots[1].ShotNo != 2 {
		t.Fatalf("镜号应重排为 1,2：%d,%d", sb.Shots[0].ShotNo, sb.Shots[1].ShotNo)
	}
	if sb.GlobalStyle != "水墨" {
		t.Fatalf("全片风格丢了: %q", sb.GlobalStyle)
	}
	if len(sb.Shots[0].Entities) != 1 || sb.Shots[0].Entities[0].Name != "唐僧" {
		t.Fatalf("实体不对: %+v", sb.Shots[0].Entities)
	}
	if sb.Shots[0].StartState == "" || sb.Shots[0].EndState == "" {
		t.Fatal("运动边界（起止状态）不能丢：下游靠它衔接上下镜")
	}
}

func TestParseStoryboard_没有镜头就是失败(t *testing.T) {
	// ⚠️ 0 个镜头不是"成功但为空"：用户拿到一个空脚本却被扣了费。
	for _, raw := range []string{`{"shots":[]}`, `{"globalStyle":"x"}`} {
		if _, err := ParseStoryboard(raw); err == nil {
			t.Errorf("%s 应判失败", raw)
		}
	}
}

func TestParseStoryboard_顶层数组也认(t *testing.T) {
	// 模型常常直接回一个镜头列表。
	sb, err := ParseStoryboard(`[{"description":"一","durationSec":5}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(sb.Shots) != 1 {
		t.Fatalf("应解出 1 个镜头: %+v", sb.Shots)
	}
}

func TestParseStoryboard_时长夹住而不是报错(t *testing.T) {
	// ⚠️ 为一个用户随手就能改的字段让整批分镜作废（并触发退款重来）不划算。
	// 真正该报错的是结构性问题。
	sb, err := ParseStoryboard(
		`{"shots":[{"description":"a","durationSec":0},{"description":"b","durationSec":999}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Shots[0].DurationSec != 5 {
		t.Fatalf("0 秒应回落到默认 5：%d", sb.Shots[0].DurationSec)
	}
	if sb.Shots[1].DurationSec != MaxShotSeconds {
		t.Fatalf("超长应夹到 %d：%d", MaxShotSeconds, sb.Shots[1].DurationSec)
	}
}

func TestParseStoryboard_镜头太多要拒(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"shots":[`)
	for i := 0; i < MaxShots+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"description":"x","durationSec":5}`)
	}
	b.WriteString(`]}`)
	if _, err := ParseStoryboard(b.String()); err == nil {
		t.Fatalf("超过 %d 镜应被拒", MaxShots)
	}
}

// ---------------------------------------------------------------- 实体清洗

func TestScanMentions_中文没有词边界(t *testing.T) {
	// ⚠️ 这是老系统最典型的一个坑：`\p{L}` 会把「@唐僧牵着马走在山路上」
	// 整串吞成一个名字，资产库里于是出现一个谁也认不出的"角色"。
	known := map[string]AssetType{"唐僧": AssetCharacter}
	got := ScanMentions("@唐僧牵着马走在山路上", known)
	if len(got) != 1 || got[0] != "唐僧" {
		t.Fatalf("应收敛成已知名「唐僧」，实际 %v", got)
	}
}

func TestScanMentions_收敛不了的超长候选丢弃(t *testing.T) {
	// 没有已知名可对齐、又明显是把整句吞了进来：丢掉比留着强。
	got := ScanMentions("@这是一段被整句吞进来的超长文字内容啊", nil)
	if len(got) != 0 {
		t.Fatalf("超长且无法收敛应丢弃，实际 %v", got)
	}
}

func TestScanMentions_标点切断(t *testing.T) {
	got := ScanMentions("@小明，@小红。", nil)
	if len(got) != 2 || got[0] != "小明" || got[1] != "小红" {
		t.Fatalf("应按标点切断: %v", got)
	}
}

func TestParseStoryboard_优先用模型给的结构化实体(t *testing.T) {
	// ⚠️ 正则是**兜底不是主路径**：它的分类表只覆盖得了现代题材，
	// 天宫、金箍棒会被一股脑判成 character。结构化产出里模型自己标了类型。
	sb, err := ParseStoryboard(`{"shots":[{"description":"@金箍棒 在发光","durationSec":5,
	  "entities":[{"name":"金箍棒","type":"prop"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Shots[0].Entities[0].Type != AssetProp {
		t.Fatalf("应按模型标的类型：%v", sb.Shots[0].Entities[0].Type)
	}
}

func TestParseStoryboard_没有结构化实体才回落正则(t *testing.T) {
	sb, err := ParseStoryboard(`{"shots":[{"description":"@小明 出场","durationSec":5}],
	  "assets":[{"name":"小明","type":"character"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(sb.Shots[0].Entities) != 1 || sb.Shots[0].Entities[0].Name != "小明" {
		t.Fatalf("应从描述里扫出来: %+v", sb.Shots[0].Entities)
	}
}

func TestParseStoryboard_超长实体名丢弃(t *testing.T) {
	sb, err := ParseStoryboard(`{"shots":[{"description":"x","durationSec":5,
	  "entities":[{"name":"这是一个长得离谱的实体名字啊啊啊","type":"character"},
	              {"name":"小明","type":"character"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(sb.Shots[0].Entities) != 1 || sb.Shots[0].Entities[0].Name != "小明" {
		t.Fatalf("超长名应被丢弃: %+v", sb.Shots[0].Entities)
	}
}

func TestParseStoryboard_档案去重且稳定排序(t *testing.T) {
	// 排序让 jsonb 快照稳定：不排的话同样的内容每次写进去都不一样，
	// diff 与审计全是噪声。
	sb, err := ParseStoryboard(`{"shots":[{"description":"x","durationSec":5}],
	  "assets":[{"name":"乙","type":"scene"},{"name":"甲","type":"character"},
	            {"name":"甲","type":"prop"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(sb.Assets) != 2 {
		t.Fatalf("同名应去重: %+v", sb.Assets)
	}
	if sb.Assets[0].Name != "乙" && sb.Assets[0].Name != "甲" {
		t.Fatalf("排序异常: %+v", sb.Assets)
	}
	if sb.Assets[0].Name > sb.Assets[1].Name {
		t.Fatalf("应按名字排序: %+v", sb.Assets)
	}
}
