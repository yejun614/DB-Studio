//go:build dockerlive

package provision

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"dbstudio/internal/crypto"
	"dbstudio/internal/docker"
	"dbstudio/internal/store"
)

// 러너가 만드는 것부터 지우는 것까지 한 바퀴.
//
// 계획을 세우는 것은 plan_test.go 가 본다. 여기서 보는 것은 그 계획대로
// **도커가 움직이는가**, 그리고 그 결과가 **우리 표에 제대로 적히는가** 다.
// 이 둘 사이가 어긋나면 화면은 "도는 중"이라고 하는데 붙을 수는 없다.
//
// 저장소를 가짜로 두지 않고 실제 SQLite 를 쓴다. 진짜를 안 쓰면 포인터 필드의
// 부분 갱신(진행률을 적는 사이에 오류가 지워지지 않는가)이 확인되지 않고,
// 그 어긋남은 실패한 이유가 사라지는 모양으로 나타난다.
func TestLiveRunnerFullCycle(t *testing.T) {
	cl := docker.New(docker.DefaultSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if st := cl.Status(ctx); !st.Reachable {
		t.Skipf("도커에 닿지 못했습니다: %s", st.Reason)
	}

	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("비밀 상자: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "live.db"), box)
	if err != nil {
		t.Fatalf("저장소: %v", err)
	}
	defer st.Close()
	proj, err := st.CreateProject(ctx, store.SaveProjectParams{Name: "라이브"})
	if err != nil {
		t.Fatalf("프로젝트: %v", err)
	}

	name := fmt.Sprintf("live-run-%d", time.Now().UnixNano()%1000000)
	plan, err := Build(Spec{Kind: "postgres", Name: name, Values: map[string]string{
		"password": "pw1234!", "database": "appdb",
		"persist": "true", "hostPort": "0", "memoryMB": "512",
		"maxConnections": "50",
	}})
	if err != nil {
		t.Fatalf("계획: %v", err)
	}

	in, err := st.CreateDBInstance(ctx, store.CreateDBInstanceParams{
		ProjectID: proj.ID, Name: name, Kind: "postgres",
		Image: plan.Image, Version: "17-alpine",
		ContainerName: plan.Container, VolumeName: plan.Volume,
		Port: plan.Port, Values: map[string]string{"database": "appdb"},
		Secrets: map[string]string{"password": "pw1234!"},
		Actor:   "검사",
	})
	if err != nil {
		t.Fatalf("줄 만들기: %v", err)
	}

	runner := NewRunner(cl, st, 30*time.Second)
	// 뒷정리는 무슨 일이 있어도 한다. 검사가 실패해도 컨테이너와 볼륨이 남으면
	// 다음 실행이 이름 충돌로 실패한다.
	defer func() {
		bg, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		cur, err := st.GetDBInstance(bg, in.ID)
		if err != nil {
			return
		}
		if err := runner.Remove(bg, cur, false); err != nil {
			t.Logf("뒷정리: %v", err)
		}
	}()

	if err := runner.Create(in.ID, plan); err != nil {
		t.Fatalf("만들기: %v", err)
	}
	// 두 번 누르는 것은 막아야 한다. 막지 않으면 두 번째가 409 로 실패하면서
	// 잘 되고 있는 첫 번째를 failed 로 덮는다.
	if err := runner.Create(in.ID, plan); !errors.Is(err, ErrBusy) {
		t.Errorf("두 번째 만들기 = %v (기대 ErrBusy)", err)
	}

	done := waitStatus(t, ctx, st, in.ID, store.InstanceRunning, store.InstanceFailed)
	if done.Status != store.InstanceRunning {
		t.Fatalf("상태 = %s\n오류: %s", done.Status, done.Error)
	}
	if done.HostPort <= 0 {
		t.Errorf("호스트 포트 = %d — 실제로 잡힌 포트를 못 읽었습니다", done.HostPort)
	}
	if done.Health != "healthy" {
		t.Errorf("건강 = %q (기대 healthy)", done.Health)
	}
	if done.ContainerID == "" {
		t.Fatal("컨테이너 ID 가 비어 있습니다")
	}
	if done.Progress != "" {
		t.Errorf("다 됐는데 진행률이 남아 있습니다: %q", done.Progress)
	}
	t.Logf("떴다: 포트 %d, 컨테이너 %s", done.HostPort, done.ContainerID[:12])

	// 로그를 읽을 수 있어야 한다. 화면에서 로그를 보는 것이 이 기능의 절반이다.
	stream, err := runner.Logs(ctx, done, docker.LogOptions{Tail: 20})
	if err != nil {
		t.Fatalf("로그: %v", err)
	}
	lines := 0
	for range stream.Lines() {
		lines++
	}
	stream.Close()
	if lines == 0 {
		t.Error("로그가 한 줄도 없습니다")
	}
	t.Logf("로그 %d줄", lines)

	// 멈추기 → 시작하기 → 다시 시작하기.
	if err := runner.Stop(ctx, done); err != nil {
		t.Fatalf("멈추기: %v", err)
	}
	stopped, _ := st.GetDBInstance(ctx, in.ID)
	// 일부러 멈춘 것은 stopped 다. 도커는 멈춘 컨테이너의 마지막 헬스체크
	// 결과(거의 언제나 unhealthy)를 들고 있어서, 그것을 그대로 적으면
	// 일부러 멈춘 DB 가 "이상 있음"으로 보인다.
	if stopped.Status != store.InstanceStopped {
		t.Errorf("멈춘 뒤 상태 = %s (기대 stopped)", stopped.Status)
	}
	if stopped.Health != "" {
		t.Errorf("멈춘 뒤 건강 = %q — 멈춘 컨테이너의 건강 상태는 뜻이 없습니다", stopped.Health)
	}
	// 멈춰도 앞서 잡아 둔 포트는 남아야 한다. 멈춘 컨테이너는 포트를 놓으므로
	// 도커에게 물어보면 빈 값이 오고, 그것으로 0 을 덮으면 다시 시작한 뒤에도
	// 붙을 수 없는 커넥션이 남는다.
	if stopped.HostPort != done.HostPort {
		t.Errorf("멈춘 뒤 포트 = %d (기대 %d)", stopped.HostPort, done.HostPort)
	}

	if err := runner.Start(ctx, stopped); err != nil {
		t.Fatalf("시작: %v", err)
	}
	started, _ := st.GetDBInstance(ctx, in.ID)
	if started.Status != store.InstanceRunning {
		t.Errorf("시작한 뒤 상태 = %s", started.Status)
	}

	if err := runner.Restart(ctx, started); err != nil {
		t.Fatalf("다시 시작: %v", err)
	}
	again, _ := st.GetDBInstance(ctx, in.ID)
	if again.Status != store.InstanceRunning {
		t.Errorf("다시 시작한 뒤 상태 = %s", again.Status)
	}

	// 사람이 도커에서 직접 지웠을 때. 우리 줄이 그것을 따라가야 한다 —
	// 없는 컨테이너를 "도는 중"으로 보여 주면 시작·멈춤이 모두 실패한다.
	if err := cl.RemoveContainer(ctx, again.ContainerID, true); err != nil {
		t.Fatalf("직접 지우기: %v", err)
	}
	if err := runner.Refresh(ctx, again); err != nil {
		t.Fatalf("새로 고치기: %v", err)
	}
	gone, _ := st.GetDBInstance(ctx, in.ID)
	if gone.Status != store.InstanceRemoved {
		t.Errorf("도커에서 사라진 뒤 상태 = %s (기대 removed)", gone.Status)
	}
	if gone.ContainerID != "" {
		t.Errorf("없는 컨테이너 ID 가 남아 있습니다: %s", gone.ContainerID)
	}

	// 볼륨은 아직 있어야 한다. 컨테이너가 사라진 것과 데이터가 사라진 것은
	// 다른 일이다.
	if err := cl.RemoveVolume(ctx, plan.Volume, false); err != nil {
		t.Errorf("볼륨이 남아 있지 않습니다: %v", err)
	}
}

// 남의 컨테이너는 건드리지 않는다.
//
// 이름이 부딪혔을 때 조용히 지우면, 사람이 직접 만든 DB 가 사라진다. 우리
// 라벨이 없는 것은 지우지 않고 알려 주어야 한다.
func TestLiveRunnerRefusesForeignContainer(t *testing.T) {
	cl := docker.New(docker.DefaultSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if st := cl.Status(ctx); !st.Reachable {
		t.Skipf("도커에 닿지 못했습니다: %s", st.Reason)
	}
	if err := cl.PullImage(ctx, "alpine:3", nil); err != nil {
		t.Fatalf("내려받기: %v", err)
	}

	name := fmt.Sprintf("dbstudio-live-foreign-%d", time.Now().UnixNano()%1000000)
	res, err := cl.CreateContainer(ctx, name, &docker.CreateRequest{
		Image: "alpine:3", Cmd: []string{"sleep", "600"},
		// 라벨을 붙이지 않는다. 남이 만든 것이라는 뜻이다.
	})
	if err != nil {
		t.Fatalf("만들기: %v", err)
	}
	defer func() { _ = cl.RemoveContainer(context.Background(), res.ID, true) }()

	r := NewRunner(cl, nil, 0)
	err = r.removeStale(ctx, name)
	if err == nil {
		t.Fatal("남의 컨테이너를 지웠습니다")
	}
	t.Logf("거절했다: %v", err)

	// 아직 있어야 한다.
	if _, err := cl.InspectContainer(ctx, res.ID); err != nil {
		t.Errorf("컨테이너가 사라졌습니다: %v", err)
	}
}

// waitStatus는 상태가 기다리는 것 중 하나가 될 때까지 되풀어 읽는다.
func waitStatus(t *testing.T, ctx context.Context, st *store.Store, id string, want ...string) *store.DBInstance {
	t.Helper()
	seen := ""
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		in, err := st.GetDBInstance(ctx, id)
		if err != nil {
			t.Fatalf("읽기: %v", err)
		}
		// 진행률이 바뀔 때마다 적어 둔다. 실패했을 때 어디까지 갔는지가 보인다.
		if in.Progress != "" && in.Progress != seen {
			seen = in.Progress
			t.Logf("… %s", seen)
		}
		for _, w := range want {
			if in.Status == w {
				return in
			}
		}
		time.Sleep(time.Second)
	}
	in, _ := st.GetDBInstance(ctx, id)
	t.Fatalf("시간 안에 끝나지 않았습니다 (상태: %s, 진행: %s)", in.Status, in.Progress)
	return nil
}
