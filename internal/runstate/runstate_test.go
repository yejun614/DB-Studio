package runstate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBeginEndCycle은 정상 종료가 표식을 지우는지 확인한다.
// 표식이 남아 있는 것 자체가 "비정상 종료"의 근거이므로, 정상 경로에서 반드시 지워져야 한다.
func TestBeginEndCycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	run, err := Begin(path, "v1", ":8080")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	m, err := Read(path)
	if err != nil || m == nil {
		t.Fatalf("표식을 읽지 못했습니다: %v", err)
	}
	if m.PID != os.Getpid() {
		t.Errorf("pid = %d, %d를 기대했습니다", m.PID, os.Getpid())
	}
	if m.Version != "v1" || m.Addr != ":8080" {
		t.Errorf("표식 내용 = %+v", m)
	}

	run.End()
	after, err := Read(path)
	if err != nil {
		t.Fatalf("read after end: %v", err)
	}
	if after != nil {
		t.Error("정상 종료 후에도 표식이 남아 있습니다")
	}
}

// TestReadMissingIsClean은 파일이 없으면 "정상 종료"로 읽히는지 확인한다.
func TestReadMissingIsClean(t *testing.T) {
	m, err := Read(filepath.Join(t.TempDir(), "none"))
	if err != nil {
		t.Fatalf("err = %v, 없는 파일은 오류가 아니어야 합니다", err)
	}
	if m != nil {
		t.Errorf("marker = %+v, nil을 기대했습니다", m)
	}
}

// TestReadCorruptKeepsTheFact는 깨진 표식도 "있었다"는 사실을 잃지 않는지 확인한다.
// 내용을 못 읽는 것과 비정상 종료가 아닌 것은 다르다.
func TestReadCorruptKeepsTheFact(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{쓰레기"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Read(path)
	if err != nil {
		t.Fatalf("깨진 파일에서 오류를 냈습니다: %v", err)
	}
	if m == nil {
		t.Fatal("깨진 표식을 '없음'으로 처리했습니다 (비정상 종료를 놓친다)")
	}
	if m.HeartbeatAt.IsZero() {
		t.Error("파일 수정 시각으로 대체하지 않았습니다")
	}
	if d := Describe(m); d == "" {
		t.Error("설명이 비었습니다")
	}
}

// TestLooksLiveForCurrentProcess는 살아 있는 프로세스(자기 자신)를 알아보는지 확인한다.
func TestLooksLiveForCurrentProcess(t *testing.T) {
	m := Marker{PID: os.Getpid(), HeartbeatAt: time.Now()}
	if !m.LooksLive() {
		t.Error("현재 프로세스를 살아 있다고 판단하지 못했습니다")
	}
}

// TestLooksLiveFalseForStaleHeartbeat는 심장박동이 오래된 표식을 죽은 것으로 보는지 확인한다.
// PID가 재사용되면 PID만으로는 오판하므로 시간을 함께 본다.
func TestLooksLiveFalseForStaleHeartbeat(t *testing.T) {
	m := Marker{PID: os.Getpid(), HeartbeatAt: time.Now().Add(-10 * HeartbeatInterval)}
	if m.LooksLive() {
		t.Error("오래된 표식을 살아 있다고 판단했습니다")
	}
}

// TestLooksLiveFalseForDeadProcess는 종료된 PID를 죽은 것으로 보는지 확인한다.
//
// 이 검사가 중요한 이유: Windows의 os.FindProcess는 종료된 PID에도 성공을 돌려준다.
// 그것에 의존하면 강제 종료된 프로세스를 "실행 중"으로 오판하고,
// 사용자에게 "다른 인스턴스가 있다"는 엉뚱한 안내를 하게 된다.
func TestLooksLiveFalseForDeadProcess(t *testing.T) {
	// 짧게 살고 끝나는 프로세스를 만들어 그 PID를 쓴다.
	cmd := deadProcessPID(t)
	m := Marker{PID: cmd, HeartbeatAt: time.Now()}
	if m.LooksLive() {
		t.Errorf("종료된 프로세스(pid=%d)를 살아 있다고 판단했습니다", cmd)
	}
}

// TestHeartbeatUpdatesMarker는 심장박동이 마지막 생존 시각을 갱신하는지 확인한다.
// 이 값이 "몇 시까지 살아 있었는가"의 근거가 된다.
func TestHeartbeatUpdatesMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	run, err := Begin(path, "v1", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer run.End()

	first, _ := Read(path)

	// 주기를 기다리지 않고 write를 직접 호출해 갱신 동작만 확인한다.
	time.Sleep(10 * time.Millisecond)
	run.m.HeartbeatAt = time.Now()
	if err := run.write(); err != nil {
		t.Fatalf("write: %v", err)
	}

	second, _ := Read(path)
	if !second.HeartbeatAt.After(first.HeartbeatAt) {
		t.Errorf("심장박동이 갱신되지 않았습니다: %v → %v",
			first.HeartbeatAt, second.HeartbeatAt)
	}
	if second.StartedAt.Equal(second.HeartbeatAt) {
		t.Error("시작 시각과 마지막 생존 시각이 같습니다 (시작 시각이 덮였습니다)")
	}
}

// TestHeartbeatStopsWithContext는 컨텍스트가 끝나면 심장박동이 멈추는지 확인한다.
func TestHeartbeatStopsWithContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	run, err := Begin(path, "v1", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer run.End()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { run.Heartbeat(ctx); close(done) }()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("컨텍스트 취소 후에도 심장박동이 멈추지 않았습니다")
	}
}

// TestMarkerIsValidJSON은 표식이 사람이 읽을 수 있는 형식인지 확인한다.
// 장애 조사에서 이 파일을 직접 열어 보는 경우가 있다.
func TestMarkerIsValidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	run, err := Begin(path, "v1.2.3", "127.0.0.1:9999")
	if err != nil {
		t.Fatal(err)
	}
	defer run.End()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("표식이 JSON이 아닙니다: %v (%s)", err, data)
	}
	for _, key := range []string{"pid", "version", "addr", "startedAt", "heartbeatAt"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("표식에 %q 가 없습니다: %s", key, data)
		}
	}
}
