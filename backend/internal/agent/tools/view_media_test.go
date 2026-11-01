package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

type vmSessions struct{ s repo.Session }

func (v vmSessions) GetByID(_ context.Context, id int64) (repo.Session, error) {
	if id != v.s.ID {
		return repo.Session{}, repo.ErrNotFound
	}
	return v.s, nil
}

type vmImages struct {
	got []struct{ ws, id int64 }
	err error
}

func (v *vmImages) ReasoningImageLimit(context.Context, string) (int, error) { return 4, nil }
func (v *vmImages) InlineImage(_ context.Context, ws, id int64) (string, error) {
	v.got = append(v.got, struct{ ws, id int64 }{ws, id})
	if v.err != nil {
		return "", v.err
	}
	return fmt.Sprintf("data:image/jpeg;base64,IMG%d", id), nil
}

func vmTool(t *testing.T, scene domain.Scene, products []string) (ViewMedia, *vmImages) {
	t.Helper()
	si, _ := json.Marshal(app.SceneInput{Products: products, Characters: []app.SceneRef{}, Pins: []app.ScenePinEntry{}})
	img := &vmImages{}
	return ViewMedia{Sessions: vmSessions{repo.Session{ID: 5, WorkspaceID: 11, Scene: scene, SceneInput: si}},
		Images: img}, img
}

func TestViewMedia_越界报错(t *testing.T) {
	tool, img := vmTool(t, domain.SceneMarketing, []string{"9001", "9002", "9003"})
	rc := RunContext{WorkspaceID: 11, SessionID: 5}
	for _, args := range []string{`{"slot":"product","index":0}`, `{"slot":"product","index":4}`,
		`{"slot":"character","index":1}`, `{"slot":"product","index":1,"mediaId":"123"}`} {
		if _, err := tool.Run(context.Background(), rc, json.RawMessage(args)); err == nil {
			t.Errorf("%s 应报错", args)
		} else if strings.Contains(args, `"index":4`) && !strings.Contains(err.Error(), "共 3 张") {
			t.Errorf("越界错误应说清共几张：%v", err)
		}
	}
	if len(img.got) != 0 {
		t.Errorf("参数非法时不应取图：%v", img.got)
	}
}

func TestViewMedia_只取本会话商品槽里的图(t *testing.T) {
	tool, img := vmTool(t, domain.SceneMarketing, []string{"9001", "9002", "9003"})
	res, err := tool.Run(context.Background(), RunContext{WorkspaceID: 11, SessionID: 5},
		json.RawMessage(`{"slot":"product","index":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.AttachImages, []string{"data:image/jpeg;base64,IMG9002"}) {
		t.Fatalf("应附 #2：%v", res.AttachImages)
	}
	if len(img.got) != 1 || img.got[0].ws != 11 || img.got[0].id != 9002 {
		t.Fatalf("应按工作区取 scene_input 里第 2 张：%v", img.got)
	}
	if strings.Contains(string(res.Content), "IMG") || !strings.Contains(string(res.Content), "商品图 #2") {
		t.Errorf("工具结果只说已附上，不含图：%s", res.Content)
	}
	// 别的工作区的运行上下文 / 通用会话 / 取不到图：都是工具错误。
	if _, err := tool.Run(context.Background(), RunContext{WorkspaceID: 12, SessionID: 5},
		json.RawMessage(`{"slot":"product","index":1}`)); err == nil {
		t.Error("工作区对不上应拒绝")
	}
	gen, _ := vmTool(t, domain.SceneGeneral, []string{"9001"})
	if _, err := gen.Run(context.Background(), RunContext{WorkspaceID: 11, SessionID: 5},
		json.RawMessage(`{"slot":"product","index":1}`)); err == nil {
		t.Error("通用会话没有商品图")
	}
	img.err = errors.New("s3: NoSuchKey bucket/obj/key")
	_, err = tool.Run(context.Background(), RunContext{WorkspaceID: 11, SessionID: 5},
		json.RawMessage(`{"slot":"product","index":1}`))
	if err == nil || strings.Contains(err.Error(), "NoSuchKey") {
		t.Errorf("取不到图应报错且不回存储细节：%v", err)
	}
}
