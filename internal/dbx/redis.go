package dbx

import (
	"context"
	"crypto/tls"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"dbstudio/internal/model"
)

func init() { register(&redisAdapter{}) }

// redisAdapter는 Redis(단일/클러스터) 접속을 담당한다.
// 스키마 개념이 없으므로 ERD/마이그레이션은 지원하지 않고 모니터링과 SLOWLOG만 다룬다.
type redisAdapter struct{}

func (a *redisAdapter) Kind() model.DBKind { return model.KindRedis }
func (a *redisAdapter) Capabilities() Capabilities {
	return Capabilities{Introspect: true, Monitor: true, Logs: true, Migrate: false, ERD: false, Explore: true}
}
func (a *redisAdapter) DefaultPort() int { return 6379 }

func (a *redisAdapter) Validate(t Target) error {
	if t.Conn == nil {
		return fmt.Errorf("커넥션 정보가 없습니다")
	}
	if strings.TrimSpace(t.Conn.Host) == "" {
		return fmt.Errorf("호스트를 입력하세요")
	}
	if db := strings.TrimSpace(t.Conn.DatabaseName); db != "" {
		n, err := strconv.Atoi(db)
		if err != nil || n < 0 {
			return fmt.Errorf("Redis DB 인덱스는 0 이상의 정수여야 합니다")
		}
	}
	return nil
}

// client는 단일 노드 또는 클러스터 클라이언트를 만든다.
// 반환된 클라이언트는 호출부가 Close해야 한다.
func (a *redisAdapter) client(t Target) (redis.UniversalClient, error) {
	c := t.Conn
	addr := fmt.Sprintf("%s:%d", c.Host, port(c, 6379))

	var tlsCfg *tls.Config
	if useTLS, err := strconv.ParseBool(t.Opt("tls", "false")); err == nil && useTLS {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
		if skip, err := strconv.ParseBool(t.Opt("tls_skip_verify", "false")); err == nil && skip {
			tlsCfg.InsecureSkipVerify = true
		}
	}

	dbIndex := 0
	if s := strings.TrimSpace(c.DatabaseName); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("Redis DB 인덱스 형식 오류: %s", s)
		}
		dbIndex = n
	}

	if cluster, err := strconv.ParseBool(t.Opt("cluster", "false")); err == nil && cluster {
		// 클러스터 모드에서는 SELECT가 불가하므로 DB 인덱스를 무시한다.
		return redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:       []string{addr},
			Username:    t.Username(),
			Password:    t.Password(),
			TLSConfig:   tlsCfg,
			DialTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second,
			ClientName:  "dbstudio",
		}), nil
	}
	return redis.NewClient(&redis.Options{
		Addr:        addr,
		Username:    t.Username(),
		Password:    t.Password(),
		DB:          dbIndex,
		TLSConfig:   tlsCfg,
		DialTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second,
		ClientName:  "dbstudio",
	}), nil
}

func (a *redisAdapter) Ping(ctx context.Context, t Target) (*ServerInfo, error) {
	client, err := a.client(t)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	start := time.Now()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("접속 실패: %w", err)
	}
	latency := time.Since(start)

	info := &ServerInfo{
		Version:  "unknown",
		Latency:  latency,
		LatencyM: float64(latency.Microseconds()) / 1000,
		Extra:    map[string]string{},
	}
	// INFO server는 대부분의 환경에서 허용되지만 관리형 서비스에서 막힐 수 있다.
	if raw, err := client.Info(ctx, "server").Result(); err == nil {
		for line := range strings.SplitSeq(raw, "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			switch key {
			case "redis_version":
				info.Version = value
			case "redis_mode", "os", "role":
				info.Extra[key] = value
			}
		}
	}
	return info, nil
}

func (a *redisAdapter) Redacted(t Target) string {
	c := t.Conn
	db := c.DatabaseName
	if db == "" {
		db = "0"
	}
	return fmt.Sprintf("redis://%s:***@%s:%d/%s", orDash(t.Username()), c.Host, port(c, 6379), db)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
