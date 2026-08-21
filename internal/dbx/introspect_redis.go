package dbx

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/schema"
)

// 키스페이스 스캔 상한. 운영 DB에서 전체 키를 훑으면 위험하므로 표본만 본다.
const (
	redisScanLimit = 20000
	redisScanBatch = 500
	// 접두사 그룹이 이보다 많으면 상위 N개만 남긴다.
	redisMaxGroups = 200
)

// Introspect는 키를 표본 스캔해 접두사별 그룹으로 요약한다.
//
// Redis에는 스키마가 없다. 대신 "user:123:profile" 같은 키 관례에서
// 논리적 구조를 읽어내는 것이 실무에서 필요한 정보다. 각 그룹을 Table로,
// 관찰된 값 타입을 Column으로 표현해 다른 DB와 같은 화면에서 볼 수 있게 한다.
func (a *redisAdapter) Introspect(ctx context.Context, t Target) (*schema.Schema, error) {
	client, err := a.client(t)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	s := &schema.Schema{
		Dialect:    string(a.Kind()),
		Shape:      schema.ShapeKeyspace,
		Name:       "db" + t.Conn.DatabaseName,
		CapturedAt: time.Now().UTC(),
		Tables:     []*schema.Table{},
		Views:      []*schema.View{},
	}

	delimiter := t.Opt("key_delimiter", ":")

	type group struct {
		prefix   string
		count    int
		types    map[string]int
		ttlCount int
		samples  []string
	}
	groups := map[string]*group{}
	scanned := 0
	truncated := false

	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, "*", redisScanBatch).Result()
		if err != nil {
			return nil, fmt.Errorf("키 스캔 실패: %w", err)
		}
		for _, key := range keys {
			scanned++
			prefix := redisKeyPrefix(key, delimiter)
			g := groups[prefix]
			if g == nil {
				g = &group{prefix: prefix, types: map[string]int{}, samples: []string{}}
				groups[prefix] = g
			}
			g.count++
			if len(g.samples) < 3 {
				g.samples = append(g.samples, key)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
		if scanned >= redisScanLimit {
			truncated = true
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("키 스캔 중단: %w", err)
		}
	}

	if truncated {
		s.AddNote("키가 많아 %d개까지만 스캔했습니다. 아래 요약은 전체가 아닌 표본입니다", scanned)
	}
	if scanned == 0 {
		s.AddNote("키스페이스가 비어 있습니다")
	}

	// 그룹별 값 타입과 TTL은 대표 샘플에만 물어본다.
	// 모든 키에 TYPE/TTL을 호출하면 왕복 횟수가 키 수만큼 늘어난다.
	list := make([]*group, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].prefix < list[j].prefix
	})
	if len(list) > redisMaxGroups {
		s.AddNote("접두사 그룹이 %d개라 상위 %d개만 표시합니다", len(list), redisMaxGroups)
		list = list[:redisMaxGroups]
	}

	for _, g := range list {
		for _, key := range g.samples {
			if typ, err := client.Type(ctx, key).Result(); err == nil {
				g.types[typ]++
			}
			if ttl, err := client.TTL(ctx, key).Result(); err == nil && ttl > 0 {
				g.ttlCount++
			}
		}
	}

	for _, g := range list {
		tbl := &schema.Table{
			Name:        g.prefix,
			Comment:     fmt.Sprintf("키 %d개 (예: %s)", g.count, strings.Join(g.samples, ", ")),
			Columns:     []*schema.Column{},
			Indexes:     []*schema.Index{},
			ForeignKeys: []*schema.ForeignKey{},
			Checks:      []*schema.Check{},
			Options: map[string]string{
				"sampledKeys": strconv.Itoa(len(g.samples)),
			},
			RowEstimate: int64(g.count),
		}
		if g.ttlCount > 0 {
			tbl.Options["hasTTL"] = "true"
		}
		// 관찰된 값 타입을 컬럼처럼 표현한다.
		typeNames := make([]string, 0, len(g.types))
		for name := range g.types {
			typeNames = append(typeNames, name)
		}
		sort.Strings(typeNames)
		for j, name := range typeNames {
			tbl.Columns = append(tbl.Columns, &schema.Column{
				Name:     name,
				Position: j + 1,
				Type:     schema.LogicalType{Base: redisValueType(name)},
				RawType:  name,
				Nullable: true,
				Presence: float64(g.types[name]) / float64(len(g.samples)),
			})
		}
		s.Tables = append(s.Tables, tbl)
	}

	// 서버 전체 키스페이스 통계를 노트로 남긴다.
	if info, err := client.Info(ctx, "keyspace").Result(); err == nil {
		for line := range strings.SplitSeq(info, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "db") {
				s.AddNote("서버 보고: %s", line)
			}
		}
	}
	return s, nil
}

// redisKeyPrefix는 키를 구분자 기준으로 잘라 그룹 이름을 만든다.
// 숫자로만 된 세그먼트는 ID로 보고 * 로 치환한다 —
// "user:1:profile"과 "user:2:profile"이 같은 그룹으로 묶여야 구조가 드러난다.
func redisKeyPrefix(key, delimiter string) string {
	if delimiter == "" {
		delimiter = ":"
	}
	parts := strings.Split(key, delimiter)
	if len(parts) == 1 {
		return key
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if isNumericOrID(p) {
			out = append(out, "*")
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, delimiter)
}

// isNumericOrID는 세그먼트가 식별자처럼 보이는지 판별한다.
func isNumericOrID(s string) bool {
	if s == "" {
		return false
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return true
	}
	// UUID 형태 (8-4-4-4-12)
	if len(s) == 36 && strings.Count(s, "-") == 4 {
		return true
	}
	// 24자 hex (MongoDB ObjectID를 키에 쓰는 경우)
	if len(s) == 24 && isHex(s) {
		return true
	}
	return false
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func redisValueType(name string) schema.BaseType {
	switch name {
	case "string":
		return schema.TypeText
	case "list", "set", "zset":
		return schema.TypeArray
	case "hash":
		return schema.TypeDocument
	case "stream":
		return schema.TypeArray
	}
	return schema.TypeUnknown
}
