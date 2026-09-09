package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// 컨테이너에 파일 넣기.
//
// ── 왜 필요한가 ─────────────────────────────────────────────────────
// 설정을 환경변수로 못 받는 DB 가 있다. ClickHouse 의 메모리 설정이 그렇다 —
// config.d 아래의 XML 로만 받는다.
//
// 호스트의 파일을 마운트하는 길은 쓸 수 없다. 이 앱이 컨테이너로 돌면 우리가
// 보는 파일 계통과 도커 데몬이 보는 것이 다르다 — 우리 컨테이너 안에 파일을
// 만들어 그 경로를 마운트하라고 하면, 데몬은 **호스트의** 그 경로를 찾고
// 거기에는 아무것도 없다. 조용히 빈 디렉터리가 마운트되는 것이 최악이다.
//
// 그래서 도커에 직접 넣는다. `docker cp` 가 쓰는 길이고, 넣는 것은 tar 다.
//
// ── 시작하기 전에 넣어야 한다 ───────────────────────────────────────
// 시작한 뒤에 넣으면 DB 가 설정을 이미 읽고 지나간 뒤다. 그래서 만들기와
// 시작하기를 갈라 두었다(CreateContainer 의 주석 참고).

// PutFile은 컨테이너 안에 파일 하나를 만든다.
//
// 경로의 디렉터리는 이미 있어야 한다. 이미지가 만들어 둔 자리에 넣는 것이
// 이 함수의 용도라(config.d 등), 없는 디렉터리를 만들어 주면 오타를 조용히
// 받아들이게 된다 — 그러면 DB 는 설정 없이 뜨고 우리는 넣었다고 믿는다.
func (c *Client) PutFile(ctx context.Context, id, filePath, body string) error {
	if !strings.HasPrefix(filePath, "/") {
		return fmt.Errorf("파일을 넣지 못했습니다: 절대 경로여야 합니다 (%s)", filePath)
	}
	dir, name := path.Split(path.Clean(filePath))
	if name == "" {
		return fmt.Errorf("파일을 넣지 못했습니다: 파일 이름이 없습니다 (%s)", filePath)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o644,
		Size: int64(len(body)),
		// 시각을 넣어 두는 이유: 비워 두면 유닉스 0 이 되고, 그 파일을 보는
		// 사람에게 1970년으로 보인다.
		ModTime: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("파일을 넣지 못했습니다: %w", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		return fmt.Errorf("파일을 넣지 못했습니다: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("파일을 넣지 못했습니다: %w", err)
	}

	// path 는 **디렉터리**다. tar 안의 이름이 파일 이름이 된다.
	q := url.Values{}
	q.Set("path", dir)
	// noOverwriteDirNonDir 를 켜지 않는다. 같은 이름을 다시 넣는 것이 정상이고
	// (설정을 고쳐 다시 만든다), 그때 실패하면 사람이 컨테이너를 지워야 한다.
	target := "/" + APIVersion + "/containers/" + url.PathEscape(id) +
		"/archive?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://docker"+target, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("파일을 넣지 못했습니다: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-tar")

	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("파일 넣기: %w", ctx.Err())
		}
		return c.unreachable(err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		// 400 은 대개 경로가 없다는 뜻이다. 그 사실을 적어 준다 —
		// "bad request" 만 보면 무엇이 잘못됐는지 알 수 없다.
		err := decodeError(res, "파일 넣기 ("+filePath+")")
		if res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w — 컨테이너 안에 %s 디렉터리가 있는지 확인하세요", err, dir)
		}
		return err
	}
	return nil
}
