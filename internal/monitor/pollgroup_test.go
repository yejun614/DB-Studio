package monitor

import (
	"context"
	"sync/atomic"
	"testing"

	"dbstudio/internal/dbx"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// countingAdapter는 대상 DB에 몇 번 물었는지 센다.
type countingAdapter struct {
	fakeAdapter
	calls atomic.Int64
}

func (a *countingAdapter) Metrics(context.Context, dbx.Target) (*metric.Set, error) {
	a.calls.Add(1)
	set := metric.NewSet()
	set.Gauge(metric.NameUp, 1, metric.UnitCount)
	return set, nil
}

// 같은 DB를 두 프로젝트가 각자 등록해도 폴러는 한 번만 물어본다.
//
// 프로젝트마다 서버를 따로 등록하므로 같은 운영 DB가 두 번 올라올 수 있다. 그것을
// 그대로 두면 30초마다 같은 DB에 두 번 물어보고, 부하는 프로젝트 수만큼 늘어난다.
// 저장·룰 평가는 커넥션마다 따로 한다 — 각 프로젝트가 자기 이력과 룰을 갖는다.
func TestSameTargetIsPolledOnce(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// 두 프로젝트가 같은 호스트·같은 DB를 각자 등록한다.
	same := func(project, name string) *model.Connection {
		pw := "pw"
		_, conn, err := st.CreateServerWithDatabase(ctx,
			store.SaveServerParams{
				ProjectID: project, Name: name, Kind: model.KindMySQL,
				Host: "10.0.0.7", Port: 3306, DefaultEnvironment: model.EnvDev,
				Options: model.Options{}, Tags: []string{}, Enabled: true,
				Username: "root", Password: &pw,
			},
			store.SaveConnectionParams{
				Name: name, Environment: model.EnvDev, DatabaseName: "appdb",
				Tags: []string{}, Enabled: true,
			})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return conn
	}
	a, err := st.CreateProject(ctx, store.SaveProjectParams{Name: "결제"})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	b, err := st.CreateProject(ctx, store.SaveProjectParams{Name: "물류"})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	first := same(a.ID, "결제-운영")
	second := same(b.ID, "물류-운영")

	adapter := &countingAdapter{fakeAdapter: fakeAdapter{kind: model.KindMySQL}}
	m := NewManager(st, DefaultConfig())
	m.pollGroup(ctx, []*model.Connection{first, second}, adapter)

	if n := adapter.calls.Load(); n != 1 {
		t.Errorf("대상 DB에 %d번 물었습니다. 같은 대상이면 한 번이어야 합니다", n)
	}

	// 그래도 두 커넥션 모두 자기 이력을 갖는다. 한쪽만 저장되면 다른 프로젝트의
	// 화면은 "수집된 적 없음"으로 남는다.
	states, err := st.ListConnectionStates(ctx)
	if err != nil {
		t.Fatalf("states: %v", err)
	}
	for _, conn := range []*model.Connection{first, second} {
		if states[conn.ID] == nil {
			t.Errorf("%s 에 저장된 상태가 없습니다", conn.Name)
			continue
		}
		names, err := st.ListMetricNames(ctx, conn.ID)
		if err != nil {
			t.Fatalf("metric names %s: %v", conn.Name, err)
		}
		if len(names) == 0 {
			t.Errorf("%s 에 저장된 지표가 없습니다", conn.Name)
		}
	}
}

// 계정이 다르면 따로 묻는다.
//
// 같은 서버라도 계정에 따라 보이는 지표가 다르다(권한에 따라 일부 뷰가 막힌다).
// 한쪽 결과를 다른 쪽에 나눠 주면 자기 계정으로는 볼 수 없는 값을 보게 된다.
func TestDifferentAccountIsPolledSeparately(t *testing.T) {
	root := &model.Connection{
		Kind: model.KindMySQL, Host: "10.0.0.7", Port: 3306,
		DatabaseName: "appdb", Username: "root",
	}
	app := &model.Connection{
		Kind: model.KindMySQL, Host: "10.0.0.7", Port: 3306,
		DatabaseName: "appdb", Username: "app",
	}
	if pollTarget(root) == pollTarget(app) {
		t.Error("계정이 다른데 같은 묶음이 되었습니다")
	}

	twin := *root
	if pollTarget(root) != pollTarget(&twin) {
		t.Error("같은 대상·같은 계정인데 다른 묶음이 되었습니다")
	}
}
