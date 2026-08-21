package dbx

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	_ "github.com/go-sql-driver/mysql"  // "mysql"
	_ "github.com/jackc/pgx/v5/stdlib"  // "pgx"
	_ "github.com/microsoft/go-mssqldb" // "sqlserver"
	_ "github.com/sijms/go-ora/v2"      // "oracle"
	_ "modernc.org/sqlite"              // "sqlite"

	"dbstudio/internal/model"
)

// 전 단계 기능이 아직 구현되지 않았어도 어댑터가 무엇을 지원할 수 있는지는 여기서 선언한다.
// UI는 이 값으로 기능 버튼을 켜고 끈다.
var relationalCaps = Capabilities{Introspect: true, Monitor: true, Logs: true, Migrate: true, ERD: true}

// SQLite는 서버가 없어 로그/쿼리 통계 소스가 존재하지 않는다.
// Logs를 true로 두면 UI가 빈 화면을 보여주므로 명시적으로 끈다.
var sqliteCaps = Capabilities{Introspect: true, Monitor: true, Logs: false, Migrate: true, ERD: true}

func init() {
	register(&sqlAdapter{
		kind: model.KindMySQL, driver: "mysql", defaultPort: 3306, caps: relationalCaps,
		versionQuery: "SELECT VERSION()", needsHost: true, needsDatabase: false,
		dsn: mysqlDSN, introspect: introspectMySQL, metrics: metricsMySQL, logs: logsMySQL,
	})
	register(&sqlAdapter{
		kind: model.KindPostgres, driver: "pgx", defaultPort: 5432, caps: relationalCaps,
		versionQuery: "SELECT version()", needsHost: true, needsDatabase: true,
		dsn: postgresDSN, introspect: introspectPostgres, metrics: metricsPostgres, logs: logsPostgres,
	})
	register(&sqlAdapter{
		kind: model.KindMSSQL, driver: "sqlserver", defaultPort: 1433, caps: relationalCaps,
		versionQuery: "SELECT @@VERSION", needsHost: true, needsDatabase: false,
		dsn: mssqlDSN, introspect: introspectMSSQL, metrics: metricsMSSQL, logs: logsMSSQL,
	})
	register(&sqlAdapter{
		kind: model.KindOracle, driver: "oracle", defaultPort: 1521, caps: relationalCaps,
		// product_component_version은 일반 사용자도 대체로 조회할 수 있다.
		versionQuery:  "SELECT version FROM product_component_version WHERE rownum = 1",
		needsHost:     true,
		needsDatabase: true,
		dsn:           oracleDSN,
		introspect:    introspectOracle,
		metrics:       metricsOracle,
		logs:          logsOracle,
	})
	register(&sqlAdapter{
		kind: model.KindSQLite, driver: "sqlite", defaultPort: 0, caps: sqliteCaps,
		versionQuery: "SELECT sqlite_version()", needsHost: false, needsDatabase: true,
		dsn: sqliteDSN, introspect: introspectSQLite, metrics: metricsSQLite,
	})
}

// mysqlDSN: user:pass@tcp(host:port)/db?params
func mysqlDSN(t Target) (string, error) {
	c := t.Conn
	var b strings.Builder
	if u := t.Username(); u != "" {
		b.WriteString(u)
		if pw := t.Password(); pw != "" {
			b.WriteString(":")
			b.WriteString(pw)
		}
		b.WriteString("@")
	}
	fmt.Fprintf(&b, "tcp(%s:%d)/%s", c.Host, port(c, 3306), c.DatabaseName)

	params := url.Values{}
	// timeout은 컨텍스트와 별개로 드라이버 레벨 상한을 둔다.
	params.Set("timeout", "10s")
	params.Set("readTimeout", "30s")
	params.Set("parseTime", "true")
	if tls := t.Opt("tls", ""); tls != "" {
		params.Set("tls", tls)
	}
	if extra := t.Opt("params", ""); extra != "" {
		parsed, err := url.ParseQuery(strings.TrimPrefix(extra, "?"))
		if err != nil {
			return "", fmt.Errorf("추가 파라미터 형식 오류: %w", err)
		}
		for k, vs := range parsed {
			for _, v := range vs {
				params.Set(k, v)
			}
		}
	}
	b.WriteString("?")
	b.WriteString(params.Encode())
	return b.String(), nil
}

// postgresDSN: postgres://user:pass@host:port/db?sslmode=...
func postgresDSN(t Target) (string, error) {
	c := t.Conn
	u := &url.URL{
		Scheme: "postgres",
		Host:   fmt.Sprintf("%s:%d", c.Host, port(c, 5432)),
		Path:   "/" + c.DatabaseName,
	}
	if user := t.Username(); user != "" {
		if pw := t.Password(); pw != "" {
			u.User = url.UserPassword(user, pw)
		} else {
			u.User = url.User(user)
		}
	}
	q := url.Values{}
	q.Set("sslmode", t.Opt("sslmode", "prefer"))
	q.Set("connect_timeout", "10")
	if sp := t.Opt("search_path", ""); sp != "" {
		q.Set("search_path", sp)
	}
	if app := t.Opt("application_name", "dbstudio"); app != "" {
		q.Set("application_name", app)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// mssqlDSN: sqlserver://user:pass@host:port?database=db&...
func mssqlDSN(t Target) (string, error) {
	c := t.Conn
	host := c.Host
	if inst := t.Opt("instance", ""); inst != "" {
		host = host + "\\" + inst
	}
	u := &url.URL{
		Scheme: "sqlserver",
		Host:   fmt.Sprintf("%s:%d", host, port(c, 1433)),
	}
	if user := t.Username(); user != "" {
		if pw := t.Password(); pw != "" {
			u.User = url.UserPassword(user, pw)
		} else {
			u.User = url.User(user)
		}
	}
	q := url.Values{}
	if c.DatabaseName != "" {
		q.Set("database", c.DatabaseName)
	}
	q.Set("dial timeout", "10")
	q.Set("app name", "dbstudio")
	// go-mssqldb 1.x는 기본 encrypt=true다. 개발용 컨테이너는 자체 서명 인증서를 쓰므로
	// 사용자가 명시하지 않으면 암호화는 켜고 인증서 검증은 완화한다.
	q.Set("encrypt", t.Opt("encrypt", "true"))
	q.Set("TrustServerCertificate", t.Opt("trust_server_certificate", "true"))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// oracleDSN: oracle://user:pass@host:port/service  (SID 방식은 옵션으로 전환)
func oracleDSN(t Target) (string, error) {
	c := t.Conn
	u := &url.URL{
		Scheme: "oracle",
		Host:   fmt.Sprintf("%s:%d", c.Host, port(c, 1521)),
		Path:   "/" + c.DatabaseName,
	}
	if user := t.Username(); user != "" {
		if pw := t.Password(); pw != "" {
			u.User = url.UserPassword(user, pw)
		} else {
			u.User = url.User(user)
		}
	}
	q := url.Values{}
	if strings.EqualFold(t.Opt("connect_type", "service"), "sid") {
		// go-ora는 SID 접속을 CONNECTION_TYPE 파라미터가 아닌 SID 쿼리로 받는다.
		u.Path = ""
		q.Set("SID", c.DatabaseName)
	}
	if ssl := t.Opt("ssl", ""); ssl != "" {
		q.Set("SSL", ssl)
	}
	if wallet := t.Opt("wallet", ""); wallet != "" {
		q.Set("WALLET", wallet)
	}
	q.Set("TIMEOUT", "10")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// sqliteDSN: 파일 경로 + PRAGMA
func sqliteDSN(t Target) (string, error) {
	path := strings.TrimSpace(t.Conn.DatabaseName)
	if path == "" {
		return "", fmt.Errorf("SQLite 파일 경로를 입력하세요")
	}
	dsn := strings.ReplaceAll(path, "\\", "/") + "?_pragma=busy_timeout(5000)"
	if ro, _ := strconv.ParseBool(t.Opt("readonly", "false")); ro {
		dsn += "&mode=ro"
	}
	return dsn, nil
}
