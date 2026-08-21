package api

import (
	"testing"

	"dbstudio/internal/model"
)

// DB만 고치는 요청은 접속 정보를 보내지 않는다.
// 빈 값을 "지우려 한다"로 읽으면 이름만 바꾸려던 요청이 서버 수정으로 번지고,
// 종류가 비어 있어 400으로 거절된다 — 화면에서는 "저장이 안 된다"로만 보인다.
func TestWantsServerChange(t *testing.T) {
	existing := &model.Connection{
		Kind: model.KindMySQL, Host: "10.0.0.1", Port: 3306, Username: "app",
		Options: model.Options{"tls": "true"},
	}
	pw := "new"

	cases := []struct {
		name string
		req  connectionRequest
		want bool
	}{
		{"DB 속성만 보낸 요청", connectionRequest{
			Name: "새 이름", Environment: model.EnvDev, DatabaseName: "appdb",
		}, false},
		{"같은 값을 되돌려 보낸 폼", connectionRequest{
			Kind: model.KindMySQL, Host: "10.0.0.1", Port: 3306, Username: "app",
			Options: model.Options{"tls": "true"},
		}, false},
		{"비밀번호를 명시하면 바꾸려는 것", connectionRequest{Password: &pw}, true},
		{"호스트 변경", connectionRequest{Host: "10.0.0.2"}, true},
		{"포트 변경", connectionRequest{Port: 3307}, true},
		{"계정 변경", connectionRequest{Username: "other"}, true},
		{"종류 변경", connectionRequest{Kind: model.KindPostgres}, true},
		{"옵션 변경", connectionRequest{Options: model.Options{"tls": "false"}}, true},
		{"옵션 없음은 변경이 아니다", connectionRequest{Options: model.Options{}}, false},
		{"extra가 있으면 자격증명 변경", connectionRequest{Extra: map[string]string{"ca": "x"}}, true},
	}
	for _, tc := range cases {
		if got := wantsServerChange(&tc.req, existing); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
