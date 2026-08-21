package dbx

import (
	"context"
	"fmt"
	"time"

	"dbstudio/internal/model"
	"dbstudio/internal/schema"
)

// 이 파일은 "관계형 모델로는 표현할 수 없는 것"을 위한 통로다.
//
// MongoDB와 Redis도 스키마 IR로 읽을 수 있지만(P3), 그것은 다른 DB와 나란히 비교하기
// 위한 최소 공통 표현이다. 실제로 이 두 DB를 운영할 때 필요한 정보 — 컬렉션별 저장
// 크기, 사용되지 않는 인덱스, 키 접두사별 분포, TTL이 없는 키, 메모리 정책 — 은
// 테이블·컬럼 모델에 억지로 밀어넣으면 오히려 읽기 어려워진다.
//
// 그래서 Adapter 인터페이스를 키우지 않고 선택적 인터페이스로 분리했다.
// 관계형 어댑터 5종은 이 파일을 알 필요가 없고, 지원 여부는 Capabilities.Explore로
// 화면에 전달된다.

// Explore는 DB 종류별 특화 조회 결과다. Shape에 따라 한 필드만 채워진다.
type Explore struct {
	Kind       model.DBKind `json:"kind"`
	Shape      schema.Shape `json:"shape"`
	CapturedAt time.Time    `json:"capturedAt"`
	// Notes는 읽지 못한 항목과 그 이유를 담는다. 권한이 부족해 일부만 읽히는 것은
	// 이 두 DB에서 흔한 일이므로, 조용히 빈 값을 보여주면 오해를 만든다.
	Notes    []string         `json:"notes,omitempty"`
	Document *DocumentExplore `json:"document,omitempty"`
	Keyspace *KeyspaceExplore `json:"keyspace,omitempty"`
}

func (e *Explore) AddNote(format string, args ...any) {
	e.Notes = append(e.Notes, fmt.Sprintf(format, args...))
}

// ---------- 문서 DB (MongoDB) ----------

type DocumentExplore struct {
	Database    string                `json:"database"`
	Server      map[string]string     `json:"server,omitempty"`
	Stats       DocumentDBStats       `json:"stats"`
	SampleSize  int                   `json:"sampleSize"`
	Collections []*DocumentCollection `json:"collections"`
}

type DocumentDBStats struct {
	Collections int   `json:"collections"`
	Views       int   `json:"views"`
	Objects     int64 `json:"objects"`
	DataSize    int64 `json:"dataSize"`
	StorageSize int64 `json:"storageSize"`
	IndexSize   int64 `json:"indexSize"`
	AvgObjSize  int64 `json:"avgObjSize"`
	Indexes     int   `json:"indexes"`
}

type DocumentCollection struct {
	Name string `json:"name"`
	// Type은 collection / view / timeseries 다. 뷰와 시계열 컬렉션은 쓰기 방식이
	// 다르므로 같은 목록에 섞여 있으면 구분되어야 한다.
	Type        string           `json:"type"`
	ViewOn      string           `json:"viewOn,omitempty"`
	Documents   int64            `json:"documents"`
	DataSize    int64            `json:"dataSize"`
	StorageSize int64            `json:"storageSize"`
	AvgObjSize  int64            `json:"avgObjSize"`
	IndexSize   int64            `json:"indexSize"`
	Capped      bool             `json:"capped,omitempty"`
	Sampled     int              `json:"sampled"`
	Fields      []*DocumentField `json:"fields"`
	Indexes     []*DocumentIndex `json:"indexes"`
	Note        string           `json:"note,omitempty"`
}

type DocumentField struct {
	Path string `json:"path"`
	Type string `json:"type"`
	// Presence는 샘플 문서 중 이 필드가 존재한 비율(0~1)이다.
	// 스키마가 강제되지 않는 DB에서 "이 필드를 믿을 수 있는가"의 근거가 된다.
	Presence float64 `json:"presence"`
	Mixed    bool    `json:"mixed,omitempty"`
	Types    string  `json:"types,omitempty"`
}

type DocumentIndex struct {
	Name      string `json:"name"`
	Keys      string `json:"keys"`
	Unique    bool   `json:"unique,omitempty"`
	Sparse    bool   `json:"sparse,omitempty"`
	Partial   bool   `json:"partial,omitempty"`
	TTLSecond *int64 `json:"ttlSeconds,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	// Ops는 서버가 재시작된 뒤 이 인덱스가 사용된 횟수($indexStats)다.
	// nil이면 읽지 못했다는 뜻이고, 0이면 정말 쓰이지 않았다는 뜻이다 —
	// 인덱스를 지우자는 판단에서 이 둘을 구분하는 것이 중요하다.
	Ops   *int64     `json:"ops,omitempty"`
	Since *time.Time `json:"since,omitempty"`
}

// ---------- 키-값 DB (Redis) ----------

type KeyspaceExplore struct {
	Server      map[string]string   `json:"server,omitempty"`
	SelectedDB  int                 `json:"selectedDb"`
	Memory      KeyspaceMemory      `json:"memory"`
	Persistence KeyspacePersistence `json:"persistence"`
	Stats       KeyspaceStats       `json:"stats"`
	Databases   []KeyspaceDB        `json:"databases"`
	Groups      []*KeyGroup         `json:"groups"`
	BigKeys     []*KeyEntry         `json:"bigKeys"`
	Commands    []CommandStat       `json:"commands,omitempty"`
	// Scanned/Truncated는 표본의 크기와 그것이 전부인지를 알려준다.
	// 운영 DB에서 전체 키를 훑을 수는 없으므로 이 값 없이는 결과를 해석할 수 없다.
	Scanned   int  `json:"scanned"`
	Truncated bool `json:"truncated"`
}

type KeyspaceMemory struct {
	Used          int64   `json:"used"`
	Peak          int64   `json:"peak"`
	RSS           int64   `json:"rss"`
	Fragmentation float64 `json:"fragmentation"`
	MaxMemory     int64   `json:"maxMemory"`
	Policy        string  `json:"policy,omitempty"`
	Dataset       int64   `json:"dataset,omitempty"`
}

type KeyspacePersistence struct {
	AOFEnabled      bool       `json:"aofEnabled"`
	RDBLastSaveAt   *time.Time `json:"rdbLastSaveAt,omitempty"`
	RDBChangesSince int64      `json:"rdbChangesSince"`
	LastSaveStatus  string     `json:"lastSaveStatus,omitempty"`
	Loading         bool       `json:"loading,omitempty"`
}

type KeyspaceStats struct {
	Uptime           int64   `json:"uptimeSeconds"`
	ConnectedClients int64   `json:"connectedClients"`
	BlockedClients   int64   `json:"blockedClients"`
	OpsPerSec        int64   `json:"opsPerSec"`
	TotalCommands    int64   `json:"totalCommands"`
	KeyspaceHits     int64   `json:"keyspaceHits"`
	KeyspaceMisses   int64   `json:"keyspaceMisses"`
	HitRatio         float64 `json:"hitRatio"`
	ExpiredKeys      int64   `json:"expiredKeys"`
	EvictedKeys      int64   `json:"evictedKeys"`
	SlowlogLen       int64   `json:"slowlogLen"`
}

type KeyspaceDB struct {
	Index    int   `json:"index"`
	Keys     int64 `json:"keys"`
	Expires  int64 `json:"expires"`
	AvgTTLMs int64 `json:"avgTtlMs"`
}

type KeyGroup struct {
	Prefix string `json:"prefix"`
	Keys   int    `json:"keys"`
	// Types는 관찰된 값 타입별 개수다. 한 접두사에 여러 타입이 섞여 있으면
	// 키 관례가 무너졌다는 신호다.
	Types      map[string]int `json:"types"`
	WithTTL    int            `json:"withTtl"`
	Bytes      int64          `json:"bytes,omitempty"`
	SampleKeys []string       `json:"sampleKeys,omitempty"`
}

type KeyEntry struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Bytes    int64  `json:"bytes"`
	Elements int64  `json:"elements,omitempty"`
	// TTL은 남은 초다. -1은 만료 없음을 뜻한다(Redis의 규약을 그대로 쓴다).
	TTL int64 `json:"ttl"`
}

type CommandStat struct {
	Name        string  `json:"name"`
	Calls       int64   `json:"calls"`
	UsecPerCall float64 `json:"usecPerCall"`
	Rejected    int64   `json:"rejected,omitempty"`
	Failed      int64   `json:"failed,omitempty"`
}

// ---------- 선택적 인터페이스 ----------

// DocumentExplorer는 문서 DB 특화 조회를 지원하는 어댑터가 구현한다.
type DocumentExplorer interface {
	ExploreDocument(ctx context.Context, t Target) (*DocumentExplore, []string, error)
}

// KeyspaceExplorer는 키-값 DB 특화 조회를 지원하는 어댑터가 구현한다.
type KeyspaceExplorer interface {
	ExploreKeyspace(ctx context.Context, t Target) (*KeyspaceExplore, []string, error)
}

// DoExplore는 어댑터가 지원하는 특화 조회를 수행한다.
// 지원하지 않는 종류는 ErrNotImplemented를 반환한다 — 호출자가 스키마 화면으로
// 안내할 수 있어야 하므로 빈 결과가 아니라 에러로 구분한다.
func DoExplore(ctx context.Context, t Target) (*Explore, error) {
	if t.Conn == nil {
		return nil, fmt.Errorf("커넥션 정보가 없습니다")
	}
	adapter, err := Get(t.Conn.Kind)
	if err != nil {
		return nil, err
	}
	out := &Explore{Kind: t.Conn.Kind, CapturedAt: time.Now().UTC()}

	switch a := adapter.(type) {
	case DocumentExplorer:
		doc, notes, err := a.ExploreDocument(ctx, t)
		if err != nil {
			return nil, err
		}
		out.Shape = schema.ShapeDocument
		out.Document = doc
		out.Notes = notes
		return out, nil
	case KeyspaceExplorer:
		ks, notes, err := a.ExploreKeyspace(ctx, t)
		if err != nil {
			return nil, err
		}
		out.Shape = schema.ShapeKeyspace
		out.Keyspace = ks
		out.Notes = notes
		return out, nil
	}
	return nil, ErrNotImplemented
}
