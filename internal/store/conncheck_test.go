package store

import (
	"testing"
	"time"

	"dbstudio/internal/metric"
	"dbstudio/internal/model"
)

// TestConnectionCheckMergesPollerState는 커넥션의 연결 확인 칸이 "테스트" 버튼 결과와
// 폴러의 최신 상태 중 더 새 것을 보이는지 본다.
//
// 폴러는 connections 행에 쓰지 않는다(그 표는 클러스터와 D1로 복제된다). 조회가 둘을
// 합치지 않으면 커넥션 화면은 사람이 테스트를 누르기 전까지 "확인 전"으로 남는다.
func TestConnectionCheckMergesPollerState(t *testing.T) {
	ctx, st := serverFixture(t)
	pj := testProject(t, ctx, st)
	srv, err := st.CreateServer(ctx, SaveServerParams{
		ProjectID: pj.ID, Name: "srv-check", Kind: model.KindPostgres,
		Host: "10.0.0.7", Port: 5432, DefaultEnvironment: model.EnvDev, Enabled: true,
	})
	if err != nil {
		t.Fatalf("서버 생성: %v", err)
	}
	conn, err := st.CreateConnection(ctx, SaveConnectionParams{
		ProjectID: pj.ID, ServerID: srv.ID, Name: "d-check",
		Environment: model.EnvDev, DatabaseName: "d", Enabled: true,
	})
	if err != nil {
		t.Fatalf("커넥션 생성: %v", err)
	}
	if conn.LastCheckAt != nil {
		t.Fatalf("아무도 확인하지 않았는데 확인 시각이 있습니다: %v", conn.LastCheckAt)
	}

	poll := func(at time.Time, up float64, msg string) {
		t.Helper()
		set := metric.NewSet()
		set.CollectedAt = at
		set.Gauge(metric.NameUp, up, metric.UnitCount)
		if err := st.SaveSamples(ctx, conn.ID, set, msg); err != nil {
			t.Fatalf("수집 저장: %v", err)
		}
	}
	check := func(wantOK bool, wantMsg string) {
		t.Helper()
		c, err := st.GetConnection(ctx, conn.ID)
		if err != nil {
			t.Fatalf("커넥션 조회: %v", err)
		}
		if c.LastCheckAt == nil || c.LastCheckOK == nil {
			t.Fatal("연결 확인 결과가 없습니다")
		}
		if *c.LastCheckOK != wantOK || c.LastCheckMsg != wantMsg {
			t.Errorf("연결 확인 = (%v, %q), 기대 (%v, %q)", *c.LastCheckOK, c.LastCheckMsg, wantOK, wantMsg)
		}
	}

	// 폴러만 확인했다: 그 결과가 보여야 한다.
	poll(time.Now().Add(-time.Minute), 0, "접속 거부")
	check(false, "접속 거부")

	// 그 뒤에 사람이 테스트를 눌렀다: 더 새 것이 이긴다.
	if err := st.RecordConnectionCheck(ctx, conn.ID, true, ""); err != nil {
		t.Fatalf("테스트 기록: %v", err)
	}
	check(true, "")

	// 다시 폴러가 더 새 결과를 냈다.
	poll(time.Now().Add(time.Minute), 0, "시간 초과")
	check(false, "시간 초과")
}
