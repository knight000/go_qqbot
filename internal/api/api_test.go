package api

import "testing"

// 群与单聊的接口在平台侧完全隔离（连上传的富媒体都不通用），
// 路径走错就是发到另一个会话或直接 404，所以单独钉住这个映射。
func TestScenePath(t *testing.T) {
	cases := []struct {
		scene   string
		peer    string
		suffix  string
		want    string
		wantErr bool
	}{
		{scene: "group", peer: "g1", suffix: "messages", want: "/v2/groups/g1/messages"},
		{scene: "group", peer: "g1", suffix: "files", want: "/v2/groups/g1/files"},
		{scene: "c2c", peer: "u1", suffix: "messages", want: "/v2/users/u1/messages"},
		{scene: "c2c", peer: "u1", suffix: "files", want: "/v2/users/u1/files"},
		// openid 出现在路径里，必须转义：含 / 的 id 会把路径拼到别处去。
		{scene: "group", peer: "a b/c", suffix: "files", want: "/v2/groups/a%20b%2Fc/files"},
		{scene: "guild", peer: "x", suffix: "messages", wantErr: true},
		{scene: "", peer: "x", suffix: "files", wantErr: true},
	}
	for _, c := range cases {
		got, err := scenePath(c.scene, c.peer, c.suffix)
		if c.wantErr {
			if err == nil {
				t.Errorf("场景 %q 应报错，实际得到 %q", c.scene, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("场景 %q 不应报错: %v", c.scene, err)
			continue
		}
		if got != c.want {
			t.Errorf("场景 %q 路径应为 %q，实际 %q", c.scene, c.want, got)
		}
	}
}
