//go:build dockerlive

package provision

import (
	"context"
	"fmt"
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
	if err := c.EnsureNetwork(ctx, NetworkName, nil); err != nil {
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
