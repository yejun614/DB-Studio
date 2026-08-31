package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 코드 자산(js·css)은 캐시에서 꺼내기 전에 반드시 서버에 되물어야 한다.
//
// 예전에는 모든 자산에 max-age=3600 만 걸려 있었고, embed.FS 는 수정 시각이 0이라
// ETag 도 Last-Modified 도 나가지 않았다. 그래서 브라우저는 한 시간 동안 되묻지
// 않았고, 새 버전을 올려도 옛 모듈이 그대로 그려졌다 — 셸(index.html)만 no-cache 로
// 두었지만 모듈 주소가 그대로여서 아무 소용이 없었다.
//
// 그 상태는 "새 기능이 보이는데 동작이 이상하다"로 나타나고, 고친 사람도 쓰는 사람도
// 무엇이 틀렸는지 알 수 없다. 이 시험이 그 조합을 지킨다.
func TestCodeAssetsRevalidate(t *testing.T) {
	e := newTestEnv(t)
	writeWebFile(t, e, "js/main.js", "export const hi = 1;")
	writeWebFile(t, e, "css/app.css", "body { color: red; }")

	for _, path := range []string{"/js/main.js", "/css/app.css"} {
		res := getRaw(t, e, path, "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", path, res.StatusCode)
		}
		cc := res.Header.Get("Cache-Control")
		if !strings.Contains(cc, "no-cache") {
			t.Errorf("%s Cache-Control = %q, no-cache 가 있어야 합니다", path, cc)
		}
		// max-age 가 함께 있으면 브라우저는 그 시간 동안 되묻지 않는다.
		if strings.Contains(cc, "max-age") {
			t.Errorf("%s Cache-Control 에 max-age 가 있습니다: %q", path, cc)
		}
		tag := res.Header.Get("ETag")
		if tag == "" {
			t.Fatalf("%s 에 ETag 가 없습니다 — 되물어도 판단할 근거가 없습니다", path)
		}

		// 같은 내용이면 304로 끝난다. no-cache 가 "매번 다시 받는다"가 아니라는
		// 것이 이 줄의 뜻이다.
		again := getRaw(t, e, path, tag)
		if again.StatusCode != http.StatusNotModified {
			t.Errorf("%s 조건부 요청 = %d, 304여야 합니다", path, again.StatusCode)
		}
	}
}

// 글꼴·그림은 오래 캐시해도 된다. 바뀌어도 화면이 조금 다를 뿐, 코드처럼 옛것과
// 새것이 섞여 오작동하지 않는다.
func TestMediaAssetsCacheLong(t *testing.T) {
	e := newTestEnv(t)
	writeWebFile(t, e, "favicon.ico", "not really an icon")

	res := getRaw(t, e, "/favicon.ico", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("파비콘 = %d", res.StatusCode)
	}
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
		t.Errorf("파비콘 Cache-Control = %q, 오래 캐시해야 합니다", cc)
	}
}

// writeWebFile은 시험용 정적 파일을 만든다. 시험 서버의 web 루트는 데이터
// 디렉터리와 같은 임시 폴더다(newTestEnv).
func writeWebFile(t *testing.T, e *testEnv, rel, body string) {
	t.Helper()
	path := filepath.Join(e.srv.cfg.DataDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func getRaw(t *testing.T, e *testEnv, path, ifNoneMatch string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	res, err := e.srv.App().Test(req, -1)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}
