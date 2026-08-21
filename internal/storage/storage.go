// Package storage는 분산 스토리지 클러스터(하둡 HDFS·Ceph)를 다룬다.
//
// 왜 dbx가 아니라 별도 패키지인가: 이들은 데이터베이스가 아니다. 테이블도 스키마도 SQL도
// 없고, 대신 파일·풀·OSD·용량이 있다. dbx의 인터페이스(Introspect/ExecDDL/Logs)에 억지로
// 끼우면 절반이 "미지원"인 어댑터가 되고, 화면은 그 절반을 숨기는 조건문으로 채워진다.
//
// 대신 접점은 둘로 나눈다.
//   - 접속 확인과 **지표 수집**은 dbx 어댑터로 붙인다. 그래야 임계값·이벤트·알림·클러스터가
//     이미 하던 일을 그대로 한다 — "디스크가 90% 찼다"를 다시 만들 이유가 없다.
//   - 목록·탐색·관리 조작은 이 패키지의 클라이언트가 맡고, 전용 화면이 부른다.
package storage

import (
	"time"

	"dbstudio/internal/opsapi"
)

// Kind는 이 패키지가 다루는 스토리지 종류다.
const (
	KindHadoop = "hadoop"
	KindCeph   = "ceph"
)

// 공통 어휘는 opsapi에 있다. 여기서는 별칭으로 둬 이 패키지의 호출부가 그대로 읽히게 한다
// (storage.Health는 "스토리지의 상태"라는 뜻이 문장에 남는다).
type (
	Health = opsapi.Health
	Fact   = opsapi.Fact
	Config = opsapi.Config
)

const (
	HealthOK      = opsapi.HealthOK
	HealthWarn    = opsapi.HealthWarn
	HealthError   = opsapi.HealthError
	HealthUnknown = opsapi.HealthUnknown
)

// ConfigFrom은 커넥션과 자격증명에서 접속 설정을 만든다.
var ConfigFrom = opsapi.ConfigFrom

// HumanBytes는 바이트를 사람이 읽는 단위로 만든다.
var HumanBytes = opsapi.HumanBytes

// Capacity는 용량이다. 바이트로만 들고 비율은 계산해 쓴다.
type Capacity struct {
	Total     int64 `json:"total"`
	Used      int64 `json:"used"`
	Available int64 `json:"available"`
}

// UsedPercent는 0..100이다. 총량을 모르면 -1을 돌려준다.
//
// 0이 아니라 -1인 이유: "모른다"와 "비어 있다"는 전혀 다른 상태인데, 0을 돌려주면
// 화면과 임계값 판정이 둘을 구분할 수 없다.
func (c Capacity) UsedPercent() float64 {
	if c.Total <= 0 {
		return -1
	}
	return float64(c.Used) / float64(c.Total) * 100
}

// Overview는 개요 화면 한 장에 들어가는 것이다.
type Overview struct {
	Kind     string   `json:"kind"`
	Version  string   `json:"version,omitempty"`
	Health   Health   `json:"health"`
	Capacity Capacity `json:"capacity"`
	// Facts는 종류마다 다른 요약값이다(라벨 → 값). 화면은 순서를 지켜 그대로 보여준다.
	Facts []Fact `json:"facts"`
	// Notes는 "왜 이 값이 비어 있는가"다. 빈칸만 보여주면 사용자는 앱을 의심한다.
	Notes []string `json:"notes,omitempty"`
}

// Entry는 HDFS 경로 하나(파일 또는 디렉터리)다.
type Entry struct {
	Name        string    `json:"name"`
	Dir         bool      `json:"dir"`
	Size        int64     `json:"size"`
	Owner       string    `json:"owner"`
	Group       string    `json:"group"`
	Permission  string    `json:"permission"`
	Replication int       `json:"replication,omitempty"`
	ModifiedAt  time.Time `json:"modifiedAt"`
}

// PathSummary는 한 경로 아래의 합계다(GETCONTENTSUMMARY).
type PathSummary struct {
	Path        string `json:"path"`
	Files       int64  `json:"files"`
	Directories int64  `json:"directories"`
	Length      int64  `json:"length"`
	SpaceUsed   int64  `json:"spaceUsed"`
	// Quota가 -1이면 제한이 없다는 뜻이다(HDFS의 표현을 그대로 둔다).
	Quota      int64 `json:"quota"`
	SpaceQuota int64 `json:"spaceQuota"`
}

// App은 YARN 애플리케이션 하나다.
type App struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	User     string    `json:"user"`
	Queue    string    `json:"queue"`
	State    string    `json:"state"`
	Progress float64   `json:"progress"`
	StartAt  time.Time `json:"startAt"`
}

// Pool은 Ceph 풀 하나다.
type Pool struct {
	Name     string `json:"name"`
	ID       int    `json:"id"`
	Type     string `json:"type"`
	Size     int    `json:"size"`
	MinSize  int    `json:"minSize"`
	PGNum    int    `json:"pgNum"`
	Used     int64  `json:"used"`
	MaxAvail int64  `json:"maxAvail"`
	Objects  int64  `json:"objects"`
	App      string `json:"app,omitempty"`
}

// OSD는 Ceph OSD 하나다.
type OSD struct {
	ID     int     `json:"id"`
	Up     bool    `json:"up"`
	In     bool    `json:"in"`
	Host   string  `json:"host,omitempty"`
	Used   int64   `json:"used"`
	Total  int64   `json:"total"`
	Weight float64 `json:"weight"`
	Status string  `json:"status"`
}

// Bucket은 RGW(오브젝트 게이트웨이) 버킷 하나다.
type Bucket struct {
	Name   string `json:"name"`
	Owner  string `json:"owner,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Object int64  `json:"objects,omitempty"`
}
