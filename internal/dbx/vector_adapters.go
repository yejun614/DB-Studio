package dbx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/dblog"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/opsapi"
	"dbstudio/internal/schema"
	"dbstudio/internal/vector"
)

// 벡터 DB 어댑터(Qdrant·Pinecone).
//
// 이들을 어댑터로 등록하는 이유는 하둡·Ceph 와 같다: 커넥션 등록·자격증명 보관·
// 접근 권한·지표 수집·임계값·이벤트·알림이 이미 커넥션 단위로 돌아가고 있고,
// 벡터 DB 에도 그대로 필요하다. 여기에 붙이지 않으면 그 여섯 가지를 통째로 다시
// 만들어야 한다.
//
// 대신 스키마·SQL·마이그레이션 능력은 전부 꺼 둔다. 화면은 그 값을 보고 메뉴를
// 감추므로 "테이블 목록이 비어 있는 Qdrant" 같은 상태가 만들어지지 않는다.
//
// **pgvector 는 여기 없다.** 그것은 PostgreSQL 커넥션의 성질이지 종류가 아니다
// (internal/vector/pgvector.go 의 첫 주석을 보라).

func init() {
	register(&qdrantAdapter{})
	register(&pineconeAdapter{})
}

// vectorConfig는 커넥션에서 접속 설정을 만든다.
func vectorConfig(t Target, defaultPort int) opsapi.Config {
	return opsapi.ConfigFrom(t.Conn, t.Secret, defaultPort)
}

// ---------- Qdrant ----------

type qdrantAdapter struct{}

func (a *qdrantAdapter) Kind() model.DBKind { return model.KindQdrant }

func (a *qdrantAdapter) Capabilities() Capabilities {
	return Capabilities{Monitor: true, Vector: true}
}

func (a *qdrantAdapter) DefaultPort() int { return vector.QdrantDefaultPort }

func (a *qdrantAdapter) Validate(t Target) error {
	if t.Conn == nil {
		return fmt.Errorf("커넥션 정보가 없습니다")
	}
	return vectorConfig(t, a.DefaultPort()).Validate()
}

func (a *qdrantAdapter) client(t Target) *vector.Qdrant {
	return vector.NewQdrant(vectorConfig(t, a.DefaultPort()))
}

func (a *qdrantAdapter) Ping(ctx context.Context, t Target) (*ServerInfo, error) {
	start := time.Now()
	version, err := a.client(t).Ping(ctx)
	if err != nil {
		return nil, err
	}
	info := &ServerInfo{Version: version, Latency: time.Since(start)}
	info.LatencyM = float64(info.Latency.Microseconds()) / 1000
	return info, nil
}

func (a *qdrantAdapter) Introspect(context.Context, Target) (*schema.Schema, error) {
	return nil, fmt.Errorf("%w: 벡터 DB에는 표와 컬럼이 없습니다 — 벡터 화면에서 보세요",
		ErrNotImplemented)
}

func (a *qdrantAdapter) Logs(context.Context, Target, *dblog.Filter) (*dblog.Result, error) {
	return nil, fmt.Errorf("%w: Qdrant 로그 조회는 지원하지 않습니다", ErrNotImplemented)
}

func (a *qdrantAdapter) ExecDDL(context.Context, Target, []string, ExecOptions) (*ExecReport, error) {
	return nil, fmt.Errorf("%w: 벡터 DB에는 DDL이 없습니다", ErrNotImplemented)
}

func (a *qdrantAdapter) Redacted(t Target) string {
	return vectorConfig(t, a.DefaultPort()).BaseURL()
}

func (a *qdrantAdapter) Metrics(ctx context.Context, t Target) (*metric.Set, error) {
	return vectorMetrics(ctx, a.client(t))
}

// ---------- Pinecone ----------

type pineconeAdapter struct{}

func (a *pineconeAdapter) Kind() model.DBKind { return model.KindPinecone }

func (a *pineconeAdapter) Capabilities() Capabilities {
	return Capabilities{Monitor: true, Vector: true}
}

func (a *pineconeAdapter) DefaultPort() int { return vector.PineconeDefaultPort }

func (a *pineconeAdapter) Validate(t Target) error {
	if t.Conn == nil {
		return fmt.Errorf("커넥션 정보가 없습니다")
	}
	// 여기서 막는 이유: Pinecone 에는 익명 접근이 없다. 키 없이 저장하면 등록은
	// 되고 모든 화면이 401 로 끝나는데, 그 원인이 설정에 있다는 것이 오류
	// 메시지에는 드러나지 않는다.
	if strings.TrimSpace(t.Password()) == "" &&
		strings.TrimSpace(t.Opt("api_key", "")) == "" {
		return fmt.Errorf("Pinecone API 키를 비밀번호 칸에 넣으세요")
	}
	return nil
}

func (a *pineconeAdapter) client(t Target) *vector.Pinecone {
	cfg := vectorConfig(t, a.DefaultPort())
	if strings.TrimSpace(cfg.Host) == "" {
		cfg.Host = vector.PineconeControlHost
	}
	return vector.NewPinecone(cfg)
}

func (a *pineconeAdapter) Ping(ctx context.Context, t Target) (*ServerInfo, error) {
	start := time.Now()
	version, err := a.client(t).Ping(ctx)
	if err != nil {
		return nil, err
	}
	info := &ServerInfo{Version: version, Latency: time.Since(start)}
	info.LatencyM = float64(info.Latency.Microseconds()) / 1000
	return info, nil
}

func (a *pineconeAdapter) Introspect(context.Context, Target) (*schema.Schema, error) {
	return nil, fmt.Errorf("%w: 벡터 DB에는 표와 컬럼이 없습니다 — 벡터 화면에서 보세요",
		ErrNotImplemented)
}

func (a *pineconeAdapter) Logs(context.Context, Target, *dblog.Filter) (*dblog.Result, error) {
	return nil, fmt.Errorf("%w: Pinecone 로그는 콘솔에서만 볼 수 있습니다", ErrNotImplemented)
}

func (a *pineconeAdapter) ExecDDL(context.Context, Target, []string, ExecOptions) (*ExecReport, error) {
	return nil, fmt.Errorf("%w: 벡터 DB에는 DDL이 없습니다", ErrNotImplemented)
}

func (a *pineconeAdapter) Redacted(t Target) string {
	cfg := vectorConfig(t, a.DefaultPort())
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		host = vector.PineconeControlHost
	}
	return "https://" + host
}

func (a *pineconeAdapter) Metrics(ctx context.Context, t Target) (*metric.Set, error) {
	return vectorMetrics(ctx, a.client(t))
}

// ---------- 공통 지표 ----------

// vectorMetrics는 벡터 DB 의 지표를 모은다.
//
// 컬렉션 개요 한 번으로 끝나는 이유: 벡터 DB 에서 주기적으로 봐야 하는 것은
// "몇 개가 들어 있고 색인이 준비됐는가"이고, 그 둘은 같은 응답에 들어 있다.
// 접속 실패를 에러가 아니라 up=0 으로 돌려주는 규칙은 다른 어댑터와 같다.
func vectorMetrics(ctx context.Context, store vector.Store) (*metric.Set, error) {
	defer store.Close()

	set := metric.NewSet()
	start := time.Now()
	ov, err := store.Overview(ctx)
	set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.Notes = append(set.Notes, err.Error())
		return set, nil
	}
	set.Gauge(metric.NameUp, 1, metric.UnitCount)
	set.Gauge(metric.NameLatency, set.LatencyMs, metric.UnitMillis)
	set.Gauge(metric.NameVectorCollections, float64(len(ov.Collections)), metric.UnitCount)

	var points, indexed float64
	var notReady int
	var maxFullness float64 = -1
	for _, col := range ov.Collections {
		if col.Points > 0 {
			points += float64(col.Points)
		}
		if col.Indexed > 0 {
			indexed += float64(col.Indexed)
		}
		// green 이 아닌 것을 센다. unknown 은 세지 않는다 — 모른다는 것과
		// 준비되지 않았다는 것은 다르고, 모르는 것을 위험으로 올리면 임계값
		// 알림이 늘 울린다.
		if col.Status != "" && col.Status != "green" && col.Status != "unknown" {
			notReady++
		}
		if col.Fullness > maxFullness {
			maxFullness = col.Fullness
		}
	}
	set.Gauge(metric.NameVectorPoints, points, metric.UnitCount)
	if indexed > 0 {
		set.Gauge(metric.NameVectorIndexed, indexed, metric.UnitCount)
	}
	set.Gauge(metric.NameVectorNotReady, float64(notReady), metric.UnitCount)
	if maxFullness >= 0 {
		set.Gauge(metric.NameVectorFullness, maxFullness, metric.UnitPercent)
	}
	set.Notes = append(set.Notes, ov.Notes...)
	return set, nil
}

// ---------- 화면이 쓰는 통로 ----------

// VectorStore는 이 커넥션의 벡터 저장소를 연다.
//
// 호출자는 반드시 Close 해야 한다. pgvector 의 경우 이 함수가 PostgreSQL 커넥션을
// 열기 때문이다 — 닫지 않으면 화면을 열 때마다 커넥션이 하나씩 샌다.
//
// PostgreSQL 을 여기서 함께 다루는 이유: pgvector 는 종류가 아니라 성질이라
// 커넥션 종류만 보고는 "벡터를 볼 수 있는가"를 답할 수 없다. 실제로 붙어서
// 확장이 있는지 봐야 하고, 그 판단은 vector.PgVector.Ping 이 한다.
func VectorStore(t Target) (vector.Store, error) {
	if t.Conn == nil {
		return nil, fmt.Errorf("커넥션 정보가 없습니다")
	}
	switch t.Conn.Kind {
	case model.KindQdrant:
		return vector.NewQdrant(vectorConfig(t, vector.QdrantDefaultPort)), nil
	case model.KindPinecone:
		cfg := vectorConfig(t, vector.PineconeDefaultPort)
		if strings.TrimSpace(cfg.Host) == "" {
			cfg.Host = vector.PineconeControlHost
		}
		return vector.NewPinecone(cfg), nil
	case model.KindPostgres:
		a, err := Get(model.KindPostgres)
		if err != nil {
			return nil, err
		}
		sa, ok := a.(*sqlAdapter)
		if !ok {
			return nil, fmt.Errorf("PostgreSQL 어댑터를 열 수 없습니다")
		}
		db, err := sa.open(t, 3)
		if err != nil {
			return nil, err
		}
		return vector.NewPgVector(db, true), nil
	}
	return nil, fmt.Errorf("%w: %s 에서는 벡터를 볼 수 없습니다", ErrUnsupportedKind, t.Conn.Kind)
}

// SupportsVector는 이 종류에서 벡터 화면을 열어 볼 수 있는지다.
//
// "열어 볼 수 있다"이지 "볼 것이 있다"가 아니다. PostgreSQL 은 확장이 깔려 있을
// 때만 실제로 보이는데, 그것은 붙어 봐야 알 수 있다 — 목록에서 미리 감추면
// 확장을 방금 깐 사람이 그 사실을 알 방법이 없다.
func SupportsVector(kind model.DBKind) bool {
	return kind.IsVector() || kind == model.KindPostgres
}
