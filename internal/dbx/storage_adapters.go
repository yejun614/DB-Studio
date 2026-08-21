package dbx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/dblog"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/storage"
)

// 하둡·Ceph 어댑터.
//
// 이들은 데이터베이스가 아니다. 그런데도 어댑터로 등록하는 이유는 **커넥션과 모니터링**
// 때문이다. 커넥션 등록·자격증명 보관·접근 권한·지표 수집·임계값·이벤트·알림은 이미
// 커넥션 단위로 돌아가고 있고, 스토리지 클러스터에도 그대로 필요하다. 여기에 붙이지
// 않으면 그 여섯 가지를 통째로 다시 만들어야 한다.
//
// 대신 스키마·SQL·마이그레이션 능력은 전부 꺼 둔다(Capabilities). 화면은 그 값을 보고
// 메뉴를 감추므로, "테이블 목록이 비어 있는 하둡"같은 상태가 만들어지지 않는다.

func init() {
	register(&hadoopAdapter{})
	register(&cephAdapter{})
}

// ---------- 하둡 ----------

type hadoopAdapter struct{}

func (a *hadoopAdapter) Kind() model.DBKind { return model.KindHadoop }

func (a *hadoopAdapter) Capabilities() Capabilities {
	return Capabilities{Monitor: true, Storage: true}
}

func (a *hadoopAdapter) DefaultPort() int { return storage.HadoopDefaultPort }

func (a *hadoopAdapter) Validate(t Target) error {
	if t.Conn == nil {
		return fmt.Errorf("커넥션 정보가 없습니다")
	}
	cfg := storage.ConfigFrom(t.Conn, t.Secret, a.DefaultPort())
	if err := cfg.Validate(); err != nil {
		return err
	}
	if y := strings.TrimSpace(t.Opt("yarn_url", "")); y != "" &&
		!strings.HasPrefix(y, "http://") && !strings.HasPrefix(y, "https://") {
		return fmt.Errorf("YARN 주소는 http:// 또는 https:// 로 시작해야 합니다")
	}
	return nil
}

func (a *hadoopAdapter) client(t Target) *storage.Hadoop {
	return storage.NewHadoop(storage.ConfigFrom(t.Conn, t.Secret, a.DefaultPort()))
}

func (a *hadoopAdapter) Ping(ctx context.Context, t Target) (*ServerInfo, error) {
	start := time.Now()
	version, err := a.client(t).Ping(ctx)
	if err != nil {
		return nil, err
	}
	info := &ServerInfo{Version: version, Latency: time.Since(start)}
	info.LatencyM = float64(info.Latency.Microseconds()) / 1000
	info.Extra = map[string]string{"webhdfs": storage.ConfigFrom(t.Conn, t.Secret, a.DefaultPort()).BaseURL()}
	return info, nil
}

func (a *hadoopAdapter) Introspect(context.Context, Target) (*schema.Schema, error) {
	return nil, fmt.Errorf("%w: 하둡에는 스키마가 없습니다", ErrNotImplemented)
}

func (a *hadoopAdapter) Logs(context.Context, Target, *dblog.Filter) (*dblog.Result, error) {
	return nil, fmt.Errorf("%w: 하둡 로그 조회는 지원하지 않습니다", ErrNotImplemented)
}

func (a *hadoopAdapter) ExecDDL(context.Context, Target, []string, ExecOptions) (*ExecReport, error) {
	return nil, fmt.Errorf("%w: 하둡에는 DDL이 없습니다", ErrNotImplemented)
}

func (a *hadoopAdapter) Redacted(t Target) string {
	return storage.ConfigFrom(t.Conn, t.Secret, a.DefaultPort()).BaseURL()
}

// Metrics는 HDFS·YARN 지표를 수집한다.
//
// 접속 실패를 에러가 아니라 up=0으로 돌려주는 규칙은 다른 어댑터와 같다. 폴러는 그것을
// 근거로 접속 이벤트를 만들고, 에러로 올리면 "물어보지도 못했다"와 구분이 사라진다.
func (a *hadoopAdapter) Metrics(ctx context.Context, t Target) (*metric.Set, error) {
	set := metric.NewSet()
	start := time.Now()
	m, err := a.client(t).Collect(ctx)
	set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.Notes = append(set.Notes, err.Error())
		return set, nil
	}
	set.Gauge(metric.NameUp, 1, metric.UnitCount)
	set.Gauge(metric.NameLatency, set.LatencyMs, metric.UnitMillis)

	if pct := m.Capacity.UsedPercent(); pct >= 0 {
		set.Gauge(metric.NameStorageUsedPct, pct, metric.UnitPercent)
	}
	set.Gauge(metric.NameStorageTotal, float64(m.Capacity.Total), metric.UnitBytes)
	set.Gauge(metric.NameStorageUsed, float64(m.Capacity.Used), metric.UnitBytes)
	set.Gauge(metric.NameStorageHealth, m.Health.Score(), metric.UnitCount)
	set.Gauge(metric.NameStorageNodesUp, m.LiveNodes, metric.UnitCount)
	set.Gauge(metric.NameStorageNodesDown, m.DeadNodes, metric.UnitCount)
	set.Gauge(metric.NameHDFSMissingBlocks, m.MissingBlocks, metric.UnitCount)
	set.Gauge(metric.NameHDFSUnderReplicated, m.UnderRepBlocks, metric.UnitCount)
	if m.CorruptBlocks > 0 {
		set.Gauge(metric.NameHDFSCorruptBlocks, m.CorruptBlocks, metric.UnitCount)
	}
	if m.YARN != nil {
		set.Gauge(metric.NameYARNAppsRunning, float64(m.YARN.AppsRunning), metric.UnitCount)
		set.Gauge(metric.NameYARNAppsPending, float64(m.YARN.AppsPending), metric.UnitCount)
		if m.YARN.TotalMB > 0 {
			set.Gauge(metric.NameYARNMemUsedPct,
				float64(m.YARN.AllocatedMB)/float64(m.YARN.TotalMB)*100, metric.UnitPercent)
		}
	} else {
		set.Notes = append(set.Notes, "YARN 주소(yarn_url)가 없어 애플리케이션 지표는 수집하지 않았습니다")
	}
	return set, nil
}

// ---------- Ceph ----------

type cephAdapter struct{}

func (a *cephAdapter) Kind() model.DBKind { return model.KindCeph }

func (a *cephAdapter) Capabilities() Capabilities {
	return Capabilities{Monitor: true, Storage: true}
}

func (a *cephAdapter) DefaultPort() int { return storage.CephDefaultPort }

func (a *cephAdapter) Validate(t Target) error {
	if t.Conn == nil {
		return fmt.Errorf("커넥션 정보가 없습니다")
	}
	cfg := storage.ConfigFrom(t.Conn, t.Secret, a.DefaultPort())
	if err := cfg.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.User) == "" {
		// 여기서 막는 이유: Ceph 대시보드는 익명 접근이 없다. 계정 없이 저장하면
		// 등록은 되지만 모든 화면이 401로 끝나고, 그 원인이 설정에 있다는 것이
		// 오류 메시지에는 드러나지 않는다.
		return fmt.Errorf("Ceph 대시보드 계정을 입력하세요")
	}
	return nil
}

func (a *cephAdapter) client(t Target) *storage.Ceph {
	return storage.NewCeph(storage.ConfigFrom(t.Conn, t.Secret, a.DefaultPort()))
}

func (a *cephAdapter) Ping(ctx context.Context, t Target) (*ServerInfo, error) {
	start := time.Now()
	status, err := a.client(t).Ping(ctx)
	if err != nil {
		return nil, err
	}
	info := &ServerInfo{Version: status, Latency: time.Since(start)}
	info.LatencyM = float64(info.Latency.Microseconds()) / 1000
	return info, nil
}

func (a *cephAdapter) Introspect(context.Context, Target) (*schema.Schema, error) {
	return nil, fmt.Errorf("%w: Ceph에는 스키마가 없습니다", ErrNotImplemented)
}

func (a *cephAdapter) Logs(context.Context, Target, *dblog.Filter) (*dblog.Result, error) {
	return nil, fmt.Errorf("%w: Ceph 로그 조회는 지원하지 않습니다", ErrNotImplemented)
}

func (a *cephAdapter) ExecDDL(context.Context, Target, []string, ExecOptions) (*ExecReport, error) {
	return nil, fmt.Errorf("%w: Ceph에는 DDL이 없습니다", ErrNotImplemented)
}

func (a *cephAdapter) Redacted(t Target) string {
	return storage.ConfigFrom(t.Conn, t.Secret, a.DefaultPort()).BaseURL()
}

func (a *cephAdapter) Metrics(ctx context.Context, t Target) (*metric.Set, error) {
	set := metric.NewSet()
	start := time.Now()
	m, err := a.client(t).Collect(ctx)
	set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.Notes = append(set.Notes, err.Error())
		return set, nil
	}
	set.Gauge(metric.NameUp, 1, metric.UnitCount)
	set.Gauge(metric.NameLatency, set.LatencyMs, metric.UnitMillis)

	if pct := m.Capacity.UsedPercent(); pct >= 0 {
		set.Gauge(metric.NameStorageUsedPct, pct, metric.UnitPercent)
	}
	set.Gauge(metric.NameStorageTotal, float64(m.Capacity.Total), metric.UnitBytes)
	set.Gauge(metric.NameStorageUsed, float64(m.Capacity.Used), metric.UnitBytes)
	set.Gauge(metric.NameStorageHealth, m.Health.Score(), metric.UnitCount)
	set.Gauge(metric.NameStorageNodesUp, m.OSDsUp, metric.UnitCount)
	set.Gauge(metric.NameStorageNodesDown, m.OSDsTotal-m.OSDsUp, metric.UnitCount)
	set.Gauge(metric.NameCephOSDsIn, m.OSDsIn, metric.UnitCount)
	set.Gauge(metric.NameCephPGsUnclean, m.PGsBad, metric.UnitCount)
	set.Gauge(metric.NameCephPools, m.Pools, metric.UnitCount)
	return set, nil
}
