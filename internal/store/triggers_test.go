package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 트리거는 매크로와 소유자를 모두 외래키로 참조한다. 소유자 권한으로 실행되는
// 기능이므로 "소유자가 없는 트리거"는 존재해서는 안 되고, 스키마가 그것을 막는다.
func triggerFixture(t *testing.T) (context.Context, *Store, *Macro, string) {
	t.Helper()
	ctx, st := macroFixture(t)
	owner := mkUser(t, ctx, st, "owner")
	m, err := st.CreateMacro(ctx, "야간 정리", "", emptyGraph, owner, "관리자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	return ctx, st, m, owner.ID
}

func scheduleParams(macroID, ownerID string, next time.Time) SaveTriggerParams {
	return SaveTriggerParams{
		MacroID: macroID, Name: "매일 3시", Kind: TriggerSchedule, Enabled: true,
		Cron: "0 3 * * *", Timezone: "Asia/Seoul", NextRunAt: &next,
		SkipIfRunning: true, OwnerID: ownerID, OwnerName: "관리자",
		Params: map[string]any{"dryRun": true, "limit": float64(10)},
	}
}

func TestCreateTriggerRoundTrips(t *testing.T) {
	ctx, st, m, owner := triggerFixture(t)
	next := time.Date(2030, 1, 1, 3, 0, 0, 0, time.UTC)

	tr, err := st.CreateTrigger(ctx, scheduleParams(m.ID, owner, next))
	if err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	if tr.Kind != TriggerSchedule || tr.Cron != "0 3 * * *" || tr.Timezone != "Asia/Seoul" {
		t.Errorf("스케줄 필드가 다르다: %+v", tr)
	}
	if tr.NextRunAt == nil || !tr.NextRunAt.Equal(next) {
		t.Errorf("다음 실행 시각이 다르다: %v", tr.NextRunAt)
	}
	// 파라미터는 저장했다가 그대로 실행에 넘긴다. JSON 왕복에서 타입이 바뀌면
	// 매크로가 다른 값으로 돌게 된다.
	if tr.Params["dryRun"] != true || tr.Params["limit"] != float64(10) {
		t.Errorf("파라미터 왕복이 깨졌다: %+v", tr.Params)
	}
	// 목록 화면이 매크로 이름을 함께 보여줄 수 있어야 한다.
	if tr.MacroName != "야간 정리" {
		t.Errorf("매크로 이름 조인이 비었다: %q", tr.MacroName)
	}
	if !tr.Enabled || !tr.SkipIfRunning {
		t.Errorf("기본 플래그가 다르다: %+v", tr)
	}
}

func TestUpdateTriggerClearsFailureState(t *testing.T) {
	ctx, st, m, owner := triggerFixture(t)
	next := time.Now().Add(time.Hour)
	tr, err := st.CreateTrigger(ctx, scheduleParams(m.ID, owner, next))
	if err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}

	for range 3 {
		if err := st.RecordTriggerFire(ctx, tr.ID, "", "failed", "권한 없음"); err != nil {
			t.Fatalf("RecordTriggerFire: %v", err)
		}
	}
	before, _ := st.GetTrigger(ctx, tr.ID)
	if before.FailCount != 3 {
		t.Fatalf("연속 실패가 쌓이지 않았다: %d", before.FailCount)
	}

	// 설정을 고쳤다는 것은 대개 그 실패를 고쳤다는 뜻이다. 카운트가 남아 있으면
	// 고친 트리거가 몇 번 안 돌고 스스로 꺼진다.
	p := scheduleParams(m.ID, owner, next)
	p.Name = "매일 4시"
	p.Cron = "0 4 * * *"
	updated, err := st.UpdateTrigger(ctx, tr.ID, p)
	if err != nil {
		t.Fatalf("UpdateTrigger: %v", err)
	}
	if updated.FailCount != 0 || updated.LastError != "" {
		t.Errorf("수정 후에도 실패 상태가 남았다: count=%d err=%q",
			updated.FailCount, updated.LastError)
	}
	if updated.Cron != "0 4 * * *" || updated.Name != "매일 4시" {
		t.Errorf("수정이 반영되지 않았다: %+v", updated)
	}
	// 소유자는 수정으로 바뀌지 않는다 — 바뀌면 실행 권한이 바뀌는 것이다.
	if updated.OwnerID != owner {
		t.Errorf("소유자가 바뀌었다: %q", updated.OwnerID)
	}
}

func TestRecordTriggerFireResetsOnSuccess(t *testing.T) {
	ctx, st, m, owner := triggerFixture(t)
	tr, _ := st.CreateTrigger(ctx, scheduleParams(m.ID, owner, time.Now()))

	_ = st.RecordTriggerFire(ctx, tr.ID, "", "failed", "실패1")
	_ = st.RecordTriggerFire(ctx, tr.ID, "", "failed", "실패2")
	_ = st.RecordTriggerFire(ctx, tr.ID, "run-9", "success", "")

	got, _ := st.GetTrigger(ctx, tr.ID)
	// 연속 실패를 세는 것이 의도다. 누적만 하면 어쩌다 실패한 트리거가
	// 몇 달 뒤 상한에 닿아 꺼진다.
	if got.FailCount != 0 {
		t.Errorf("성공 후에도 실패 카운트가 남았다: %d", got.FailCount)
	}
	if got.LastStatus != "success" || got.LastRunID != "run-9" {
		t.Errorf("마지막 결과가 다르다: %+v", got)
	}
	if got.LastFiredAt == nil {
		t.Error("발화 시각이 기록되지 않았다")
	}
}

func TestDueTriggersOnlyPicksReadySchedules(t *testing.T) {
	ctx, st, m, owner := triggerFixture(t)
	now := time.Now()

	past := scheduleParams(m.ID, owner, now.Add(-time.Minute))
	past.Name = "지난 것"
	dueOne, _ := st.CreateTrigger(ctx, past)

	future := scheduleParams(m.ID, owner, now.Add(time.Hour))
	future.Name = "아직"
	_, _ = st.CreateTrigger(ctx, future)

	off := scheduleParams(m.ID, owner, now.Add(-time.Minute))
	off.Name = "꺼진 것"
	off.Enabled = false
	_, _ = st.CreateTrigger(ctx, off)

	// 이벤트 트리거는 next_run_at이 없으므로 절대 잡히면 안 된다.
	_, _ = st.CreateTrigger(ctx, SaveTriggerParams{
		MacroID: m.ID, Name: "이벤트", Kind: TriggerEvent, Enabled: true,
		MinIntervalSec: 300, OwnerID: owner,
	})

	due, err := st.DueTriggers(ctx, now)
	if err != nil {
		t.Fatalf("DueTriggers: %v", err)
	}
	if len(due) != 1 || due[0].ID != dueOne.ID {
		names := make([]string, len(due))
		for i, d := range due {
			names[i] = d.Name
		}
		t.Fatalf("예정된 트리거가 %v (기대: [지난 것])", names)
	}
}

func TestEventTriggersFilterByConnection(t *testing.T) {
	ctx, st, m, owner := triggerFixture(t)

	global, _ := st.CreateTrigger(ctx, SaveTriggerParams{
		MacroID: m.ID, Name: "모든 커넥션", Kind: TriggerEvent, Enabled: true,
		MinIntervalSec: 300, OwnerID: owner,
	})
	// 커넥션 FK는 실제 행을 요구하므로 여기서는 전역 트리거와 꺼진 트리거만 본다.
	_, _ = st.CreateTrigger(ctx, SaveTriggerParams{
		MacroID: m.ID, Name: "꺼짐", Kind: TriggerEvent, Enabled: false,
		MinIntervalSec: 300, OwnerID: owner,
	})

	got, err := st.EventTriggers(ctx, "conn-없음")
	if err != nil {
		t.Fatalf("EventTriggers: %v", err)
	}
	// 커넥션을 지정하지 않은 트리거는 모든 커넥션의 이벤트에 반응해야 한다.
	if len(got) != 1 || got[0].ID != global.ID {
		t.Fatalf("이벤트 트리거 결과가 %d개 (기대: 전역 하나)", len(got))
	}
}

func TestDisableTriggerWithReasonClearsSchedule(t *testing.T) {
	ctx, st, m, owner := triggerFixture(t)
	tr, _ := st.CreateTrigger(ctx, scheduleParams(m.ID, owner, time.Now().Add(-time.Minute)))

	if err := st.DisableTriggerWithReason(ctx, tr.ID, "소유자 계정이 비활성화되었습니다"); err != nil {
		t.Fatalf("DisableTriggerWithReason: %v", err)
	}
	got, _ := st.GetTrigger(ctx, tr.ID)
	if got.Enabled {
		t.Error("꺼지지 않았다")
	}
	// 예정 시각이 남아 있으면 다시 켰을 때 곧바로 한 번 실행된다.
	if got.NextRunAt != nil {
		t.Errorf("예정 시각이 남았다: %v", got.NextRunAt)
	}
	if got.LastStatus != "disabled" || got.LastError == "" {
		t.Errorf("이유가 남지 않았다: %+v", got)
	}

	// 꺼진 트리거는 예정 목록에 잡히지 않는다.
	due, _ := st.DueTriggers(ctx, time.Now())
	if len(due) != 0 {
		t.Errorf("꺼진 트리거가 예정 목록에 남았다: %d", len(due))
	}
}

func TestDeleteTriggerAndMissing(t *testing.T) {
	ctx, st, m, owner := triggerFixture(t)
	tr, _ := st.CreateTrigger(ctx, scheduleParams(m.ID, owner, time.Now()))

	if err := st.DeleteTrigger(ctx, tr.ID); err != nil {
		t.Fatalf("DeleteTrigger: %v", err)
	}
	if _, err := st.GetTrigger(ctx, tr.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("삭제된 트리거를 읽었다: %v", err)
	}
	// 없는 대상에 대한 조작은 조용히 성공하면 안 된다 — API가 404를 낼 수 없다.
	if err := st.DeleteTrigger(ctx, tr.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("두 번째 삭제가 %v", err)
	}
	if err := st.SetTriggerEnabled(ctx, "없는-id", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("없는 트리거 토글이 %v", err)
	}
	if _, err := st.UpdateTrigger(ctx, "없는-id", scheduleParams(m.ID, owner, time.Now())); !errors.Is(err, ErrNotFound) {
		t.Errorf("없는 트리거 수정이 %v", err)
	}
}

// 매크로를 지우면 그 트리거도 사라져야 한다. 남으면 스케줄러가 매번
// "매크로를 읽지 못했습니다"로 실패하며 로그만 채운다.
func TestDeletingMacroRemovesTriggers(t *testing.T) {
	ctx, st, m, owner := triggerFixture(t)
	if _, err := st.CreateTrigger(ctx, scheduleParams(m.ID, owner, time.Now())); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	if err := st.DeleteMacro(ctx, m.ID); err != nil {
		t.Fatalf("DeleteMacro: %v", err)
	}
	left, err := st.ListTriggers(ctx, "")
	if err != nil {
		t.Fatalf("ListTriggers: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("매크로를 지웠는데 트리거가 %d개 남았다", len(left))
	}
}

func TestListTriggersFiltersByMacro(t *testing.T) {
	ctx, st, m, owner := triggerFixture(t)
	other, err := st.CreateMacro(ctx, "다른 매크로", "", emptyGraph, testAuthor, "관리자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	_, _ = st.CreateTrigger(ctx, scheduleParams(m.ID, owner, time.Now()))
	_, _ = st.CreateTrigger(ctx, scheduleParams(other.ID, owner, time.Now()))

	all, _ := st.ListTriggers(ctx, "")
	if len(all) != 2 {
		t.Errorf("전체 목록이 %d개", len(all))
	}
	mine, _ := st.ListTriggers(ctx, m.ID)
	if len(mine) != 1 || mine[0].MacroID != m.ID {
		t.Errorf("매크로 필터가 동작하지 않는다: %d개", len(mine))
	}
}
