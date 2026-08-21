// Package runstate는 "실행 중" 표식을 관리해 비정상 종료를 다음 시작 때 알린다.
//
// 왜 필요한가: 강제 종료(작업 관리자, kill -9, 다른 프로세스의 Stop-Process, 전원 차단)는
// 프로세스에 아무 기회도 주지 않으므로 종료 로그가 남지 않는다. 그러면 로그는
// "시작했다"에서 그냥 끊기고, 사용자는 "이유 없이 꺼졌다"고 볼 수밖에 없다.
//
// 표식 파일을 두면 그 상황이 증거가 된다.
//   - 정상 종료: 파일을 지운다
//   - 강제 종료: 파일이 남는다 → 다음 시작 때 "이전 실행이 종료 기록을 남기지 않았다"고 알린다
//
// 심장박동(주기적 갱신)을 기록하는 이유: 파일이 남아 있다는 사실만으로는 언제 죽었는지
// 알 수 없다. 마지막 갱신 시각이 있으면 "몇 시까지는 살아 있었다"를 바로 말할 수 있다.
// 이것이 없으면 DB 파일의 수정 시각 같은 간접 증거를 뒤져야 한다.
package runstate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FileName은 데이터 디렉터리에 두는 표식 파일 이름이다.
const FileName = "dbstudio.running"

// HeartbeatInterval은 표식을 갱신하는 주기다.
//
// 1분으로 잡은 이유: 조사에 필요한 정밀도는 "분" 단위면 충분하고, 더 자주 쓰면
// 아무 일도 하지 않는 서버가 계속 디스크를 건드리게 된다.
const HeartbeatInterval = time.Minute

// Marker는 실행 중인 프로세스의 정보다.
type Marker struct {
	PID         int       `json:"pid"`
	Version     string    `json:"version"`
	Addr        string    `json:"addr"`
	StartedAt   time.Time `json:"startedAt"`
	HeartbeatAt time.Time `json:"heartbeatAt"`
}

// Age는 마지막 심장박동으로부터 지난 시간이다.
func (m Marker) Age() time.Duration { return time.Since(m.HeartbeatAt) }

// staleAfter는 심장박동이 이보다 오래되면 "그 프로세스는 이미 없다"고 보는 기준이다.
// 갱신 주기의 3배로 잡아 한두 번 놓친 경우를 살아 있는 것으로 오판하지 않는다.
const staleAfter = 3 * HeartbeatInterval

// LooksLive는 표식을 남긴 프로세스가 아직 돌고 있는 것처럼 보이는지 판단한다.
//
// 두 조건을 함께 본다.
//   - 심장박동이 최근이다: 오래된 표식은 죽은 프로세스가 남긴 것이다
//   - 그 PID가 아직 살아 있다: 표식만 최근일 수는 없다
//
// 둘을 함께 보는 이유는 PID 재사용이다. 죽은 프로세스의 PID가 다른 프로그램에
// 다시 할당되면 PID만으로는 "실행 중"으로 오판한다. 심장박동이 멈춘 지 오래면
// 그 PID의 주인은 우리가 아니다.
func (m Marker) LooksLive() bool {
	if m.PID <= 0 || m.HeartbeatAt.IsZero() {
		return false
	}
	if m.Age() > staleAfter {
		return false
	}
	return processAlive(m.PID)
}

func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Read는 표식을 읽는다. 파일이 없으면 (nil, nil)이다 — 정상 종료했다는 뜻이다.
//
// 내용이 깨진 파일도 오류로 만들지 않는다: 표식을 못 읽는 것이 서버 시작을 막을 이유는
// 없고, "이전 실행이 비정상 종료했다"는 사실 자체는 파일의 존재만으로 이미 알 수 있다.
func Read(path string) (*Marker, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		// 파일이 있었다는 사실만 살린다. 시각은 파일 수정 시각으로 대체한다.
		if st, serr := os.Stat(path); serr == nil {
			return &Marker{HeartbeatAt: st.ModTime()}, nil
		}
		return &Marker{}, nil
	}
	return &m, nil
}

// Run은 현재 프로세스의 표식이다.
type Run struct {
	path string
	m    Marker
}

// Begin은 표식을 만든다. 이전 표식이 있으면 덮어쓴다(호출 전에 Read로 확인해야 한다).
func Begin(path string, version, addr string) (*Run, error) {
	now := time.Now()
	r := &Run{path: path, m: Marker{
		PID: os.Getpid(), Version: version, Addr: addr,
		StartedAt: now, HeartbeatAt: now,
	}}
	if err := r.write(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Run) write() error {
	data, err := json.Marshal(r.m)
	if err != nil {
		return err
	}
	return os.WriteFile(r.path, data, 0o600)
}

// Heartbeat는 ctx가 끝날 때까지 표식을 주기적으로 갱신한다.
// 실패는 무시한다 — 표식을 갱신하지 못하는 것이 서비스를 멈출 이유는 아니다.
func (r *Run) Heartbeat(ctx context.Context) {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.m.HeartbeatAt = time.Now()
			_ = r.write()
		}
	}
}

// End는 표식을 지운다. 이 호출이 곧 "정상적으로 종료했다"는 기록이다.
func (r *Run) End() {
	if r == nil {
		return
	}
	_ = os.Remove(r.path)
}

// Describe는 이전 실행에 대해 로그에 적을 설명을 만든다.
func Describe(m *Marker) string {
	if m == nil {
		return ""
	}
	if m.StartedAt.IsZero() {
		return fmt.Sprintf("이전 실행 정보를 읽을 수 없습니다 (마지막 갱신 %s)",
			m.HeartbeatAt.Format(time.RFC3339))
	}
	return fmt.Sprintf("pid=%d version=%s addr=%s 시작=%s 마지막 생존=%s",
		m.PID, m.Version, m.Addr,
		m.StartedAt.Format(time.RFC3339), m.HeartbeatAt.Format(time.RFC3339))
}
