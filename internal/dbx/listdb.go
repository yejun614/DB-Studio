package dbx

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"dbstudio/internal/model"
)

// 서버에 실제로 붙어 관리 대상 DB 목록을 읽는다.
//
// 이것이 있어야 "서버 등록 → DB 골라서 한 번에 추가"가 성립한다. 이름을 손으로 적게 하면
// 오타가 나고, 오타는 접속 실패로만 드러난다 — 그런데 접속 실패는 자격증명 문제와
// 구분되지 않아 엉뚱한 곳을 뒤지게 된다.

// DatabaseInfo는 서버에서 발견한 DB 하나다.
type DatabaseInfo struct {
	Name string `json:"name"`
	// System은 DB 엔진이 스스로 쓰는 것이다(information_schema, master 등).
	// 목록에서 빼지 않고 표시만 하는 이유: 드물게 그것을 봐야 하는 사람이 있고,
	// 숨기면 "왜 안 보이지"가 된다. 대신 기본 선택에서는 빠진다.
	System bool `json:"system"`
	// Registered는 이미 등록된 DB인지다. API가 채운다.
	Registered bool `json:"registered"`
	// Note는 크기나 접근 불가 사유처럼 고르는 데 도움이 되는 한 줄이다.
	Note string `json:"note,omitempty"`
}

// ListDatabases는 서버의 DB 목록을 읽는다.
//
// t.Conn.DatabaseName은 무시하고 종류별 부트스트랩 DB로 붙는다 — 목록을 물어보는
// 시점에는 아직 대상 DB가 정해지지 않았기 때문이다.
func ListDatabases(ctx context.Context, srv *model.Server, sec *model.Secret) ([]DatabaseInfo, error) {
	a, err := Get(srv.Kind)
	if err != nil {
		return nil, err
	}
	t := TargetFromServer(srv, sec, bootstrapDatabase(srv.Kind))

	switch srv.Kind {
	case model.KindMongoDB:
		return listMongoDatabases(ctx, t)
	case model.KindRedis:
		return listRedisDatabases(ctx, t)
	case model.KindOracle:
		// Oracle에서 "DB"는 서비스/PDB이고, 그것을 나열하려면 CDB 권한이 필요하다
		// (v$pdbs). 일반 계정으로는 물어볼 수조차 없으므로 손으로 적게 한다.
		return nil, fmt.Errorf("%w: Oracle은 서비스 이름을 직접 입력해야 합니다", ErrNotImplemented)
	case model.KindSQLite:
		// SQLite는 파일 하나가 DB 하나다. 서버라는 것이 없으니 목록도 없다.
		return nil, fmt.Errorf("%w: SQLite는 파일 하나가 곧 DB입니다", ErrNotImplemented)
	}

	sa, ok := a.(*sqlAdapter)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotImplemented, srv.Kind)
	}
	return listSQLDatabases(ctx, sa, t, srv.Kind)
}

// TargetFromServer는 아직 커넥션이 없는 상태에서 접속 대상을 만든다.
//
// 서버 등록 화면(목록 조회·연결 테스트)이 쓴다. model.Connection을 임시로 빚는 이유는
// 어댑터 전체가 그 타입을 받도록 되어 있기 때문이다 — 목록 조회 하나 때문에
// 어댑터 인터페이스를 바꾸는 것은 비용이 더 크다.
func TargetFromServer(srv *model.Server, sec *model.Secret, database string) Target {
	return Target{
		Conn: &model.Connection{
			ID:           srv.ID,
			Name:         srv.Name,
			Kind:         srv.Kind,
			Environment:  srv.DefaultEnvironment,
			Host:         srv.Host,
			Port:         srv.Port,
			DatabaseName: database,
			Options:      srv.Options,
			Enabled:      true,
			ServerID:     srv.ID,
			ServerName:   srv.Name,
		},
		Secret: sec,
	}
}

// bootstrapDatabase는 "목록을 물어보기 위해 붙을 DB"다.
// 어느 엔진이든 항상 존재하는 것을 골라야 한다.
func bootstrapDatabase(kind model.DBKind) string {
	switch kind {
	case model.KindPostgres:
		return "postgres"
	case model.KindMSSQL:
		return "master"
	case model.KindMongoDB:
		return "admin"
	case model.KindRedis:
		return "0"
	default:
		// MySQL은 DB 없이도 붙을 수 있다.
		return ""
	}
}

var listDatabaseSQL = map[model.DBKind]string{
	// datistemplate으로 템플릿을 거른다. 템플릿에 붙는 것은 의미가 없고,
	// template0은 접속 자체가 막혀 있어 고르면 반드시 실패한다.
	model.KindPostgres: `SELECT datname FROM pg_database
		WHERE datistemplate = false AND has_database_privilege(datname, 'CONNECT')
		ORDER BY datname`,
	model.KindMySQL: `SHOW DATABASES`,
	// state/HAS_DBACCESS로 붙을 수 없는 것을 거른다. 복구 중이거나 오프라인인 DB를
	// 목록에 넣으면 고르는 순간 실패한다.
	model.KindMSSQL: `SELECT name FROM sys.databases
		WHERE state = 0 AND HAS_DBACCESS(name) = 1 ORDER BY name`,
}

// systemDatabases는 엔진이 스스로 쓰는 DB다. 표시하되 기본 선택에서는 뺀다.
var systemDatabases = map[model.DBKind]map[string]bool{
	model.KindPostgres: {"postgres": true, "template0": true, "template1": true},
	model.KindMySQL: {
		"information_schema": true, "mysql": true,
		"performance_schema": true, "sys": true,
	},
	model.KindMSSQL:   {"master": true, "tempdb": true, "model": true, "msdb": true},
	model.KindMongoDB: {"admin": true, "local": true, "config": true},
}

func listSQLDatabases(ctx context.Context, a *sqlAdapter, t Target, kind model.DBKind) ([]DatabaseInfo, error) {
	query, ok := listDatabaseSQL[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotImplemented, kind)
	}
	db, err := a.open(t, 1)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("DB 목록 조회 실패: %w", err)
	}
	defer rows.Close()

	out := []DatabaseInfo{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("DB 목록 읽기 실패: %w", err)
		}
		out = append(out, DatabaseInfo{Name: name, System: systemDatabases[kind][name]})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("DB 목록 읽기 실패: %w", err)
	}
	return out, nil
}

func listMongoDatabases(ctx context.Context, t Target) ([]DatabaseInfo, error) {
	uri, err := (&mongoAdapter{}).uri(t)
	if err != nil {
		return nil, err
	}
	opts := options.Client().ApplyURI(uri).
		SetServerSelectionTimeout(10 * time.Second).
		SetAppName("dbstudio")
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("접속 설정 오류: %w", err)
	}
	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	specs, err := client.ListDatabases(ctx, map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("DB 목록 조회 실패: %w", err)
	}
	out := make([]DatabaseInfo, 0, len(specs.Databases))
	for _, d := range specs.Databases {
		info := DatabaseInfo{Name: d.Name, System: systemDatabases[model.KindMongoDB][d.Name]}
		if d.SizeOnDisk > 0 {
			info.Note = formatBytes(d.SizeOnDisk)
		}
		out = append(out, info)
	}
	return out, nil
}

// listRedisDatabases는 번호가 붙은 DB를 나열한다.
//
// Redis의 "DB"는 이름이 아니라 0부터의 번호이고, 개수는 서버 설정이다.
// 관리형 Redis는 CONFIG를 막아 두는 일이 많아 INFO keyspace로 물러선다.
func listRedisDatabases(ctx context.Context, t Target) ([]DatabaseInfo, error) {
	client, err := (&redisAdapter{}).client(t)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	count := 16
	if cfg, err := client.ConfigGet(ctx, "databases").Result(); err == nil {
		if n, err := strconv.Atoi(cfg["databases"]); err == nil && n > 0 {
			count = n
		}
	}

	// keyspace 정보로 "실제로 쓰이는" DB에 표시를 남긴다.
	// 16개를 다 보여주면서 어디에 데이터가 있는지 알려주지 않으면 고를 수가 없다.
	keys := map[int]string{}
	if info, err := client.Info(ctx, "keyspace").Result(); err == nil {
		for line := range strings.SplitSeq(info, "\n") {
			line = strings.TrimSpace(line)
			idx, rest, found := strings.Cut(line, ":")
			if !found || !strings.HasPrefix(idx, "db") {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimPrefix(idx, "db")); err == nil {
				keys[n] = rest
				if n >= count {
					count = n + 1
				}
			}
		}
	}

	out := make([]DatabaseInfo, 0, count)
	for i := range count {
		info := DatabaseInfo{Name: strconv.Itoa(i)}
		if stat, ok := keys[i]; ok {
			info.Note = stat
		} else {
			info.Note = "비어 있음"
			// 비어 있는 DB를 기본 선택에서 빼기 위해 시스템과 같은 취급을 한다.
			// 0번만은 관례상 기본으로 쓰이므로 남긴다.
			info.System = i != 0
		}
		out = append(out, info)
	}
	return out, nil
}

// SortDatabases는 고르기 좋은 순서로 정렬한다: 일반 DB 먼저, 시스템 DB는 뒤로.
func SortDatabases(list []DatabaseInfo) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].System != list[j].System {
			return !list[i].System
		}
		return list[i].Name < list[j].Name
	})
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}

// CanListDatabases는 이 종류가 원격 DB 목록 조회를 지원하는지다.
// 화면이 "DB 목록 불러오기" 버튼을 그릴지 정하는 데 쓴다 —
// 눌러야만 안 된다는 것을 알게 되면 그것은 알려준 것이 아니다.
func CanListDatabases(kind model.DBKind) bool {
	switch kind {
	case model.KindPostgres, model.KindMySQL, model.KindMSSQL,
		model.KindMongoDB, model.KindRedis:
		return true
	default:
		return false
	}
}

// BootstrapDatabase는 목록을 물어보기 위해 붙을 DB를 반환한다.
func BootstrapDatabase(kind model.DBKind) string { return bootstrapDatabase(kind) }
