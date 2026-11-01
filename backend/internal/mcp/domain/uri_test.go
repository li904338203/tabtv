package domain

import "testing"

func TestMatchURITemplate(t *testing.T) {
	const tmpl = "seeaitv://projects/{projectId}/documents/{docId}"
	cases := []struct {
		uri  string
		ok   bool
		want map[string]string
	}{
		{"seeaitv://projects/11/documents/5", true, map[string]string{"projectId": "11", "docId": "5"}},
		{"seeaitv://projects/11/documents/", false, nil},
		{"seeaitv://projects//documents/5", false, nil},
		{"seeaitv://projects/11/documents/5/x", false, nil},
		{"seeaitv://projects/11/canvas", false, nil},
		{"https://projects/11/documents/5", false, nil},
	}
	for _, c := range cases {
		got, ok := MatchURITemplate(tmpl, c.uri)
		if ok != c.ok {
			t.Errorf("%s：ok=%v", c.uri, ok)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s：%s=%q", c.uri, k, got[k])
			}
		}
	}
	if _, ok := MatchURITemplate("seeaitv://media/{mediaId}", "seeaitv://media/7"); !ok {
		t.Error("末尾变量应匹配")
	}
	if _, ok := MatchURITemplate("seeaitv://x", "seeaitv://x"); !ok {
		t.Error("无变量的模板逐字相等应匹配")
	}
}
