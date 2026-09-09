//go:build dockerlive

package docker

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 실제 데몬에 붙여 컨테이너 한 살이를 돌려 본다.
//
// 태그로 갈라 둔 이유: 이 검사는 도커가 있어야 하고 이미지를 내려받고 포트를
// 잡는다. 보통의 `go test ./...` 에 섞여 있으면 도커 없는 기계에서 실패하고,
// 실패하는 검사가 하나 있으면 나머지도 함께 안 돌게 된다.
//
//	go test -tags dockerlive ./internal/docker/ -v -run TestLive
func TestLiveContainerLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	c := New(DefaultSocket)
	st := c.Status(ctx)
	if !st.Reachable {
		t.Skipf("도커에 닿지 못했습니다: %s", st.Reason)
	}
	t.Logf("도커 %s (API %s) %s/%s", st.Version, st.APIVersion, st.OS, st.Architecture)

	const image = "redis:7-alpine" // 작아서 검사에 알맞다
	name := fmt.Sprintf("dbstudio-live-%d", time.Now().UnixNano())
	labels := map[string]string{"dbstudio.test": "1", "dbstudio.name": name}

	// 1) 이미지
	had, err := c.ImageExists(ctx, image)
	if err != nil {
		t.Fatalf("이미지 확인: %v", err)
	}
	t.Logf("이미지가 이미 있나: %v", had)
	lines := 0
	if err := c.PullImage(ctx, image, func(PullProgress) { lines++ }); err != nil {
		t.Fatalf("내려받기: %v", err)
	}
	t.Logf("진행 줄 %d개", lines)

	// 2) 볼륨·네트워크
	vol := name + "-data"
	if err := c.CreateVolume(ctx, vol, labels); err != nil {
		t.Fatalf("볼륨: %v", err)
	}
	defer func() { _ = c.RemoveVolume(context.Background(), vol, true) }()
	net := "dbstudio-live-net"
	if err := c.EnsureNetwork(ctx, net, labels); err != nil {
		t.Fatalf("네트워크: %v", err)
	}
	// 두 번 불러도 실패하지 않아야 한다(이미 있으면 그대로 둔다).
	if err := c.EnsureNetwork(ctx, net, labels); err != nil {
		t.Errorf("네트워크를 두 번 만들 때 실패했습니다: %v", err)
	}

	// 3) 만들기
	res, err := c.CreateContainer(ctx, name, &CreateRequest{
		Image:        image,
		Labels:       labels,
		ExposedPorts: map[string]struct{}{"6379/tcp": {}},
		Healthcheck: &HealthConfig{
			Test:     []string{"CMD-SHELL", "redis-cli ping | grep -q PONG"},
			Interval: int64(2 * time.Second), Timeout: int64(2 * time.Second),
			Retries: 10,
		},
		HostConfig: &HostConfig{
			// 호스트 포트를 0 으로 두면 도커가 빈 포트를 골라 준다. 검사가
			// 남의 포트를 잡지 않게 하려는 것이다.
			PortBindings: map[string][]PortBinding{"6379/tcp": {{HostPort: "0"}}},
			Mounts:       []Mount{{Type: "volume", Source: vol, Target: "/data"}},
			Memory:       256 << 20,
			NanoCPUs:     500_000_000, // 0.5 코어
		},
	})
	if err != nil {
		t.Fatalf("만들기: %v", err)
	}
	id := res.ID
	defer func() { _ = c.RemoveContainer(context.Background(), id, true) }()
	t.Logf("만들었다: %s (경고 %v)", id[:12], res.Warnings)

	// 4) 시작
	if err := c.StartContainer(ctx, id); err != nil {
		t.Fatalf("시작: %v", err)
	}
	// 두 번 시작해도 오류가 아니어야 한다.
	if err := c.StartContainer(ctx, id); err != nil {
		t.Errorf("두 번 시작할 때 실패했습니다: %v", err)
	}

	// 5) 건강해질 때까지 기다린다
	var ins *Inspect
	for range 40 {
		ins, err = c.InspectContainer(ctx, id)
		if err != nil {
			t.Fatalf("살펴보기: %v", err)
		}
		if ins.HealthStatus() == "healthy" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if ins.HealthStatus() != "healthy" {
		t.Errorf("건강해지지 않았습니다: 상태=%s 건강=%s",
			ins.State.Status, ins.HealthStatus())
	}
	t.Logf("상태=%s 건강=%s 이름=%s", ins.State.Status, ins.HealthStatus(), ins.ShortName())

	// 도커가 골라 준 호스트 포트를 읽을 수 있어야 한다. 이것을 못 읽으면
	// 커넥션으로 등록할 때 무슨 포트로 붙어야 하는지 알 수 없다.
	//
	// **요청이 아니라 결과를 봐야 한다.** HostConfig.PortBindings 에는 우리가
	// 보낸 "0" 이 그대로 남아 있다 — 처음에 그것을 읽었고, 검사가 잡았다.
	if got := ins.HostPort("6379/tcp"); got == "" {
		t.Errorf("호스트 포트를 읽지 못했습니다: 요청=%+v 결과=%+v",
			ins.HostConfig.PortBindings, ins.NetworkSettings.Ports)
	} else {
		t.Logf("호스트 포트=%s (요청은 %+v 였다)", got, ins.HostConfig.PortBindings["6379/tcp"])
	}
	if ip := ins.IPAddress(); ip == "" {
		t.Error("컨테이너 IP 를 읽지 못했습니다")
	} else {
		t.Logf("컨테이너 IP=%s", ip)
	}

	// 6) 라벨로 찾기
	found, err := c.ListContainers(ctx, map[string]string{"dbstudio.name": name})
	if err != nil {
		t.Fatalf("목록: %v", err)
	}
	if len(found) != 1 {
		t.Errorf("라벨로 %d개를 찾았습니다(기대 1개)", len(found))
	}

	// 7) 로그
	logs, err := c.Logs(ctx, id, LogOptions{Tail: 5, Timestamps: true})
	if err != nil {
		t.Fatalf("로그: %v", err)
	}
	defer logs.Close()
	got := 0
	for line := range logs.Lines() {
		got++
		if got == 1 {
			t.Logf("첫 줄: [%s] %.70s", line.Stream, line.Text)
		}
	}
	if got == 0 {
		t.Error("로그를 한 줄도 읽지 못했습니다")
	}
	t.Logf("로그 %d줄", got)

	// 8) 멈추고 다시 시작
	if err := c.StopContainer(ctx, id, 5*time.Second); err != nil {
		t.Fatalf("멈추기: %v", err)
	}
	if err := c.StopContainer(ctx, id, 5*time.Second); err != nil {
		t.Errorf("두 번 멈출 때 실패했습니다: %v", err)
	}
	if err := c.RestartContainer(ctx, id, 5*time.Second); err != nil {
		t.Fatalf("다시 시작: %v", err)
	}
	ins, _ = c.InspectContainer(ctx, id)
	if !ins.State.Running {
		t.Errorf("다시 시작했는데 돌지 않습니다: %s", ins.State.Status)
	}

	// 9) 지우기 — 두 번 지워도 오류가 아니어야 한다
	if err := c.RemoveContainer(ctx, id, true); err != nil {
		t.Fatalf("지우기: %v", err)
	}
	if err := c.RemoveContainer(ctx, id, true); err != nil {
		t.Errorf("없는 것을 지울 때 실패했습니다: %v", err)
	}
}
