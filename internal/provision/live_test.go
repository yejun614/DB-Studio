//go:build dockerlive

package provision

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"dbstudio/internal/docker"
)

// 계획이 **실제로 도는 DB** 를 만드는가.
//
// 이 검사가 나머지와 다른 이유: 계획 검사는 "우리가 의도한 값이 갔는가"를 보고,
// 이것은 "그 값으로 DB 가 실제로 뜨는가"를 본다. 둘은 다른 질문이다 —
// 환경변수 이름을 하나 잘못 적으면 앞의 검사는 통과하고 DB 만 뜨지 않는다.
//
//	go test -tags dockerlive ./internal/provision/ -v -run TestLive
func TestLiveRecipesActuallyStart(t *testing.T) {
	c := docker.New(docker.DefaultSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if st := c.Status(ctx); !st.Reachable {
		t.Skipf("도커에 닿지 못했습니다: %s", st.Reason)
	}
	if err := c.EnsureNetwork(ctx, NetworkName, NetworkLabels()); err != nil {
		t.Fatalf("네트워크: %v", err)
	}

	// 가벼운 것만 돌린다. MS-SQL 은 2GB 를 요구하고 MySQL 은 초기화가 느려서
	// 검사 한 번이 몇 분이 된다 — 그 둘은 사람이 직접 확인한다.
	for _, kind := range []string{"postgres", "redis", "mongodb", "mariadb"} {
		t.Run(kind, func(t *testing.T) {
			name := fmt.Sprintf("live-%s-%d", kind, time.Now().UnixNano()%1000000)
			vals := map[string]string{
				"password": "Pw1234!aB", "database": "appdb", "username": "root",
				"persist": "false", "memoryMB": "512", "hostPort": "0",
			}
			p, err := Build(Spec{Kind: kind, Name: name, Values: vals})
			if err != nil {
				t.Fatalf("계획: %v", err)
			}
			t.Logf("이미지=%s 인자=%v", p.Image, p.Args)

			if err := c.PullImage(ctx, p.Image, nil); err != nil {
				t.Fatalf("내려받기: %v", err)
			}
			res, err := c.CreateContainer(ctx, p.Container, p.CreateRequest("live"))
			if err != nil {
				t.Fatalf("만들기: %v", err)
			}
			defer func() { _ = c.RemoveContainer(context.Background(), res.ID, true) }()
			if err := c.StartContainer(ctx, res.ID); err != nil {
				t.Fatalf("시작: %v", err)
			}

			// 건강해질 때까지 기다린다. 헬스체크가 이 레시피의 명령으로 도는지도
			// 여기서 확인된다 — 명령이 틀리면 영원히 starting 이다.
			deadline := time.Now().Add(90 * time.Second)
			var ins *docker.Inspect
			for time.Now().Before(deadline) {
				ins, err = c.InspectContainer(ctx, res.ID)
				if err != nil {
					t.Fatalf("살펴보기: %v", err)
				}
				if !ins.State.Running {
					dumpLogs(t, c, res.ID)
					t.Fatalf("컨테이너가 죽었습니다: 상태=%s 종료코드=%d %s",
						ins.State.Status, ins.State.ExitCode, ins.State.Error)
				}
				if ins.HealthStatus() == "healthy" {
					break
				}
				time.Sleep(time.Second)
			}
			if ins.HealthStatus() != "healthy" {
				dumpLogs(t, c, res.ID)
				t.Fatalf("건강해지지 않았습니다: %s", ins.HealthStatus())
			}
			port := ins.HostPort(fmt.Sprintf("%d/tcp", p.Port))
			if port == "" {
				t.Error("호스트 포트를 읽지 못했습니다")
			}
			t.Logf("건강함 · 호스트 포트=%s · 컨테이너 IP=%s", port, ins.IPAddress())
		})
	}
}

// dumpLogs는 실패했을 때 로그를 붙여 준다.
//
// 없으면 "뜨지 않았습니다"만 남고, 그 한 줄로는 환경변수 이름이 틀린 것인지
// 메모리가 부족한 것인지 알 수 없다.
func dumpLogs(t *testing.T, c *docker.Client, id string) {
	t.Helper()
	logs, err := c.Logs(context.Background(), id, docker.LogOptions{Tail: 25})
	if err != nil {
		t.Logf("로그도 읽지 못했습니다: %v", err)
		return
	}
	defer logs.Close()
	for line := range logs.Lines() {
		t.Logf("  [%s] %s", line.Stream, line.Text)
	}
}

// ClickHouse 의 설정 파일이 **실제로 먹는가.**
//
// 이것을 따로 확인하는 이유: 파일을 넣는 것과 DB 가 그것을 읽는 것은 다른
// 일이다. 경로가 하나 틀리면 파일은 잘 들어가고 DB 는 기본값으로 뜬다 —
// 오류도 없고, 우리는 설정했다고 믿는다. 이 세션에 그 함정을 두 번 만났다.
func TestLiveClickHouseConfigTakesEffect(t *testing.T) {
	c := docker.New(docker.DefaultSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if st := c.Status(ctx); !st.Reachable {
		t.Skipf("도커에 닿지 못했습니다: %s", st.Reason)
	}
	if err := c.EnsureNetwork(ctx, NetworkName, NetworkLabels()); err != nil {
		t.Fatalf("네트워크: %v", err)
	}

	name := fmt.Sprintf("live-ch-%d", time.Now().UnixNano()%1000000)
	p, err := Build(Spec{Kind: "clickhouse", Name: name, Values: map[string]string{
		"password": "Pw1234!aB", "database": "appdb", "persist": "false",
		"hostPort": "0", "memoryMB": "1024",
		"markCacheMB": "256", "maxQueryMemoryMB": "900", "maxMemoryRatio": "0.5",
	}})
	if err != nil {
		t.Fatalf("계획: %v", err)
	}
	// 파일은 둘이어야 한다. 서버 설정과 프로필 설정이 다른 디렉터리로 간다.
	if p.Files[ClickHouseServerConfig] == "" || p.Files[ClickHouseUserConfig] == "" {
		got := []string{}
		for path := range p.Files {
			got = append(got, path)
		}
		t.Fatalf("설정 파일 자리가 어긋났습니다: %v", got)
	}

	if err := c.PullImage(ctx, p.Image, nil); err != nil {
		t.Fatalf("내려받기: %v", err)
	}
	res, err := c.CreateContainer(ctx, p.Container, p.CreateRequest("live"))
	if err != nil {
		t.Fatalf("만들기: %v", err)
	}
	defer func() { _ = c.RemoveContainer(context.Background(), res.ID, true) }()

	// 시작하기 **전에** 넣는다.
	for path, body := range p.Files {
		if err := c.PutFile(ctx, res.ID, path, body); err != nil {
			t.Fatalf("파일 넣기(%s): %v", path, err)
		}
		t.Logf("넣었다: %s (%d바이트)", path, len(body))
	}
	if err := c.StartContainer(ctx, res.ID); err != nil {
		t.Fatalf("시작: %v", err)
	}

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		ins, err := c.InspectContainer(ctx, res.ID)
		if err != nil {
			t.Fatalf("살펴보기: %v", err)
		}
		if !ins.State.Running {
			dumpLogs(t, c, res.ID)
			t.Fatalf("죽었습니다: 종료코드=%d", ins.State.ExitCode)
		}
		if ins.HealthStatus() == "healthy" {
			break
		}
		time.Sleep(time.Second)
	}

	// 넣은 값이 실제로 먹었는지 서버에 물어본다. 파일이 있는 것과 읽힌 것은
	// 다르고, 그 차이가 이 검사의 이유다.
	out, err := c.Exec(ctx, res.ID, []string{
		"clickhouse-client", "--password", "Pw1234!aB", "--query",
		"SELECT value FROM system.server_settings WHERE name = 'mark_cache_size'",
	})
	if err != nil {
		dumpLogs(t, c, res.ID)
		t.Fatalf("물어보기: %v", err)
	}
	if got := strings.TrimSpace(out); got != "268435456" {
		t.Errorf("마크 캐시 = %q, 기대 268435456 (설정 파일이 먹지 않았습니다)", got)
	} else {
		t.Logf("마크 캐시 = %s — 설정 파일이 먹었다", got)
	}

	// 프로필 설정도 확인한다. 이것이 config.d 에 잘못 적히면 오류 없이 안 먹는다.
	out, err = c.Exec(ctx, res.ID, []string{
		"clickhouse-client", "--password", "Pw1234!aB", "--query",
		"SELECT value FROM system.settings WHERE name = 'max_memory_usage'",
	})
	if err != nil {
		t.Fatalf("물어보기: %v", err)
	}
	if got := strings.TrimSpace(out); got != "943718400" {
		t.Errorf("질의 메모리 상한 = %q, 기대 943718400 (프로필 설정이 먹지 않았습니다)", got)
	} else {
		t.Logf("질의 메모리 상한 = %s — 프로필 설정이 먹었다", got)
	}
}
