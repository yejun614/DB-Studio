package dbx

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"dbstudio/internal/model"
)

func init() { register(&mongoAdapter{}) }

// mongoAdapter는 MongoDB 접속을 담당한다.
// 문서 DB이므로 관계형 ERD/마이그레이션 대상은 아니고, 컬렉션 프로파일링과 모니터링만 지원한다.
type mongoAdapter struct{}

func (a *mongoAdapter) Kind() model.DBKind { return model.KindMongoDB }
func (a *mongoAdapter) Capabilities() Capabilities {
	return Capabilities{Introspect: true, Monitor: true, Logs: true, Migrate: false, ERD: false, Explore: true}
}
func (a *mongoAdapter) DefaultPort() int { return 27017 }

func (a *mongoAdapter) Validate(t Target) error {
	if t.Conn == nil {
		return fmt.Errorf("커넥션 정보가 없습니다")
	}
	if t.Opt("uri", "") == "" && strings.TrimSpace(t.Conn.Host) == "" {
		return fmt.Errorf("호스트 또는 전체 URI를 입력하세요")
	}
	_, err := a.uri(t)
	return err
}

func (a *mongoAdapter) uri(t Target) (string, error) {
	// 사용자가 전체 URI를 넣었으면 그대로 쓴다 (mongodb+srv, 다중 호스트 등).
	if raw := strings.TrimSpace(t.Opt("uri", "")); raw != "" {
		if !strings.HasPrefix(raw, "mongodb://") && !strings.HasPrefix(raw, "mongodb+srv://") {
			return "", fmt.Errorf("URI는 mongodb:// 또는 mongodb+srv:// 로 시작해야 합니다")
		}
		return raw, nil
	}
	c := t.Conn
	u := &url.URL{
		Scheme: "mongodb",
		Host:   fmt.Sprintf("%s:%d", c.Host, port(c, 27017)),
	}
	if user := t.Username(); user != "" {
		if pw := t.Password(); pw != "" {
			u.User = url.UserPassword(user, pw)
		} else {
			u.User = url.User(user)
		}
	}
	if c.DatabaseName != "" {
		u.Path = "/" + c.DatabaseName
	}
	q := url.Values{}
	if src := t.Opt("auth_source", ""); src != "" {
		q.Set("authSource", src)
	}
	mech, err := mongoAuthMechanism(t.Opt("auth_mechanism", ""))
	if err != nil {
		return "", err
	}
	if mech != "" {
		q.Set("authMechanism", mech)
	}
	if props := strings.TrimSpace(t.Opt("auth_mechanism_properties", "")); props != "" {
		q.Set("authMechanismProperties", props)
	}
	if rs := t.Opt("replica_set", ""); rs != "" {
		q.Set("replicaSet", rs)
	}
	if tls, err := strconv.ParseBool(t.Opt("tls", "false")); err == nil && tls {
		q.Set("tls", "true")
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// MongoAuthMechanisms는 고를 수 있는 인증 방식이다. 화면의 선택 목록과 여기의 검증이
// 같은 값을 보도록 한 곳에 둔다.
//
// 드라이버가 아는 값만 넣는다. 목록에 없는 값을 URI에 얹으면 오류가 접속 시점에야 나고,
// 그때의 메시지("auth mechanism ... not supported")는 어디를 고쳐야 하는지 알려주지 않는다.
var MongoAuthMechanisms = []string{
	"SCRAM-SHA-256",
	"SCRAM-SHA-1",
	"MONGODB-X509",
	"MONGODB-AWS",
	"MONGODB-OIDC",
	"GSSAPI",
	"PLAIN",
}

// mongoAuthMechanism은 입력값을 정규화한다. 빈 값은 "서버 기본값"이라는 뜻이며 그대로 통과한다.
// 대소문자만 다른 입력(scram-sha-256)은 고쳐서 받는다 — 사람이 옮겨 적는 값이고,
// 대문자 여부는 사용자가 판단할 수 있는 종류의 문제가 아니다.
func mongoAuthMechanism(raw string) (string, error) {
	v := strings.ToUpper(strings.TrimSpace(raw))
	if v == "" {
		return "", nil
	}
	for _, known := range MongoAuthMechanisms {
		if v == known {
			return v, nil
		}
	}
	return "", fmt.Errorf("알 수 없는 인증 방식 %q입니다. 가능한 값: %s",
		raw, strings.Join(MongoAuthMechanisms, ", "))
}

func (a *mongoAdapter) Ping(ctx context.Context, t Target) (*ServerInfo, error) {
	uri, err := a.uri(t)
	if err != nil {
		return nil, err
	}
	opts := options.Client().ApplyURI(uri).
		SetServerSelectionTimeout(10 * time.Second).
		SetConnectTimeout(10 * time.Second).
		SetAppName("dbstudio")

	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("접속 설정 오류: %w", err)
	}
	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	start := time.Now()
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		return nil, fmt.Errorf("접속 실패: %w", err)
	}
	latency := time.Since(start)

	info := &ServerInfo{
		Version:  "unknown",
		Latency:  latency,
		LatencyM: float64(latency.Microseconds()) / 1000,
		Extra:    map[string]string{},
	}
	// buildInfo는 권한에 따라 막힐 수 있으므로 실패를 무시한다.
	var build bson.M
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build); err == nil {
		if v, ok := build["version"].(string); ok {
			info.Version = v
		}
	}
	return info, nil
}

func (a *mongoAdapter) Redacted(t Target) string {
	if raw := strings.TrimSpace(t.Opt("uri", "")); raw != "" {
		if u, err := url.Parse(raw); err == nil {
			if u.User != nil {
				u.User = url.UserPassword(u.User.Username(), "***")
			}
			return u.String()
		}
		return "mongodb://***"
	}
	c := t.Conn
	user := t.Username()
	if user == "" {
		user = "-"
	}
	return fmt.Sprintf("mongodb://%s:***@%s:%d/%s", user, c.Host, port(c, 27017), c.DatabaseName)
}
