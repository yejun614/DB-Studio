// Package dblog은 대상 DB의 로그와 쿼리 통계를 표현하는 타입을 정의한다.
//
// dbx(수집)와 api(제공)가 모두 쓰는 타입이므로 별도 패키지로 분리했다.
//
// 로그는 성격이 다른 두 종류를 함께 다룬다:
//
//   - Entry: 시간순 로그 항목 (에러 로그, 슬로우 쿼리 로그, 프로파일러, SLOWLOG)
//     "언제 무슨 일이 있었는가"를 본다.
//   - QueryStat: 다이제스트별 누적 통계 (pg_stat_statements, performance_schema 등)
//     "어떤 쿼리가 전체적으로 비싼가"를 본다.
//
// 둘은 서로를 대체하지 못한다. 시간순 로그는 특정 시점의 사건을 찾는 데 쓰고,
// 누적 통계는 개선 대상을 고르는 데 쓴다.
package dblog

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Severity는 로그 항목의 심각도다.
type Severity string

const (
	SeverityDebug   Severity = "debug"
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
	SeverityFatal   Severity = "fatal"
)

func (s Severity) Valid() bool {
	switch s {
	case SeverityDebug, SeverityInfo, SeverityWarning, SeverityError, SeverityFatal:
		return true
	}
	return false
}

// Rank는 심각도 비교용 정수다. 필터링에서 "warning 이상"을 표현하는 데 쓴다.
func (s Severity) Rank() int {
	switch s {
	case SeverityDebug:
		return 1
	case SeverityInfo:
		return 2
	case SeverityWarning:
		return 3
	case SeverityError:
		return 4
	case SeverityFatal:
		return 5
	}
	return 0
}

// SourceKind는 로그를 어디서 읽었는지다. 화면에서 출처를 구분하고,
// 소스별로 무엇을 기대할 수 있는지 사용자가 알 수 있게 한다.
type SourceKind string

const (
	SourceSlowQuery  SourceKind = "slow_query" // 느린 쿼리 로그
	SourceErrorLog   SourceKind = "error_log"  // 서버 에러/알림 로그
	SourceProfiler   SourceKind = "profiler"   // MongoDB system.profile
	SourceSlowLog    SourceKind = "slowlog"    // Redis SLOWLOG
	SourceStatements SourceKind = "statements" // 누적 쿼리 통계
	SourceCurrent    SourceKind = "current"    // 현재 실행 중인 쿼리
)

// Entry는 시간순 로그 항목 하나다.
type Entry struct {
	At       time.Time  `json:"at"`
	Severity Severity   `json:"severity"`
	Source   SourceKind `json:"source"`
	Message  string     `json:"message"`

	// 쿼리 관련 필드. 쿼리성 로그(슬로우 쿼리, 프로파일러)에서만 채워진다.
	Query      string  `json:"query,omitempty"`
	Normalized string  `json:"normalized,omitempty"`
	Digest     string  `json:"digest,omitempty"`
	DurationMs float64 `json:"durationMs,omitempty"`

	RowsExamined int64  `json:"rowsExamined,omitempty"`
	RowsSent     int64  `json:"rowsSent,omitempty"`
	User         string `json:"user,omitempty"`
	Database     string `json:"database,omitempty"`
	Client       string `json:"client,omitempty"`

	Extra map[string]string `json:"extra,omitempty"`
}

// QueryStat은 정규화된 쿼리 하나의 누적 통계다.
type QueryStat struct {
	Digest     string `json:"digest"`
	Normalized string `json:"normalized"`
	// Sample은 실제 실행된 쿼리 예시다. 리터럴 값이 포함될 수 있다.
	Sample string `json:"sample,omitempty"`

	Calls       int64   `json:"calls"`
	TotalMs     float64 `json:"totalMs"`
	MeanMs      float64 `json:"meanMs"`
	MaxMs       float64 `json:"maxMs"`
	MinMs       float64 `json:"minMs"`
	StddevMs    float64 `json:"stddevMs,omitempty"`
	RowsTotal   int64   `json:"rowsTotal,omitempty"`
	RowsPerCall float64 `json:"rowsPerCall,omitempty"`

	// 캐시/버퍼 관련. DB마다 의미가 조금씩 다르므로 비율로 정규화해 담는다.
	CacheHitPct float64 `json:"cacheHitPct,omitempty"`
	// FirstSeen/LastSeen은 DB가 제공하는 경우에만 채워진다.
	FirstSeen *time.Time        `json:"firstSeen,omitempty"`
	LastSeen  *time.Time        `json:"lastSeen,omitempty"`
	Database  string            `json:"database,omitempty"`
	User      string            `json:"user,omitempty"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// Result는 로그 조회 결과다.
type Result struct {
	Entries []Entry     `json:"entries"`
	Stats   []QueryStat `json:"stats"`
	// Sources는 이 DB에서 실제로 읽을 수 있었던 소스 목록이다.
	Sources []SourceStatus `json:"sources"`
	// Notes는 소스를 읽지 못한 이유나 설정 안내다.
	// 로그 기능은 DB 설정에 크게 의존하므로(슬로우 로그 비활성, 확장 미설치 등)
	// "왜 안 보이는지"를 알려주는 것이 결과 자체만큼 중요하다.
	Notes []string `json:"notes,omitempty"`
	// Truncated는 상한에 걸려 일부만 반환했음을 뜻한다.
	Truncated bool `json:"truncated"`
}

// SourceStatus는 한 로그 소스의 가용 여부와 사유다.
type SourceStatus struct {
	Kind      SourceKind `json:"kind"`
	Label     string     `json:"label"`
	Available bool       `json:"available"`
	Count     int        `json:"count"`
	// Hint는 사용할 수 없을 때 활성화 방법이다.
	Hint string `json:"hint,omitempty"`
}

func NewResult() *Result {
	return &Result{
		Entries: []Entry{},
		Stats:   []QueryStat{},
		Sources: []SourceStatus{},
	}
}

// AddNote는 안내 사항을 기록한다. 같은 메시지는 중복하지 않는다.
func (r *Result) AddNote(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, n := range r.Notes {
		if n == msg {
			return
		}
	}
	r.Notes = append(r.Notes, msg)
}

// MarkSource는 소스의 가용 여부를 기록한다.
func (r *Result) MarkSource(kind SourceKind, label string, available bool, count int, hint string) {
	for i := range r.Sources {
		if r.Sources[i].Kind == kind {
			r.Sources[i].Available = available
			r.Sources[i].Count = count
			r.Sources[i].Hint = hint
			return
		}
	}
	r.Sources = append(r.Sources, SourceStatus{
		Kind: kind, Label: label, Available: available, Count: count, Hint: hint,
	})
}

// SortEntries는 최근 항목이 먼저 오도록 정렬한다.
func (r *Result) SortEntries() {
	sort.SliceStable(r.Entries, func(i, j int) bool {
		return r.Entries[i].At.After(r.Entries[j].At)
	})
}

// Filter는 로그 조회 조건이다.
type Filter struct {
	// From/To는 시간 범위다. 비어 있으면 최근 1시간을 본다.
	From time.Time
	To   time.Time
	// MinSeverity가 지정되면 그 이상만 반환한다.
	MinSeverity Severity
	// Search는 메시지/쿼리에 대한 부분 문자열 검색이다 (대소문자 구분 없음).
	Search string
	// Regex가 true면 Search를 정규식으로 해석한다.
	Regex bool
	// MinDurationMs가 0보다 크면 그보다 느린 쿼리만 반환한다.
	MinDurationMs float64
	// Sources가 비어 있지 않으면 해당 소스만 조회한다.
	Sources []SourceKind
	// Limit는 반환할 최대 항목 수다.
	Limit int
	// StatsOrderBy는 통계 정렬 기준이다: total | mean | calls | max
	StatsOrderBy string
}

// WantsSource는 이 소스를 조회해야 하는지 판단한다.
func (f *Filter) WantsSource(kind SourceKind) bool {
	if len(f.Sources) == 0 {
		return true
	}
	for _, s := range f.Sources {
		if s == kind {
			return true
		}
	}
	return false
}

// EffectiveLimit는 상한을 적용한 항목 수를 반환한다.
func (f *Filter) EffectiveLimit() int {
	if f.Limit <= 0 {
		return 200
	}
	if f.Limit > 2000 {
		return 2000
	}
	return f.Limit
}

// Normalize는 필터의 기본값을 채운다.
func (f *Filter) Normalize() {
	if f.To.IsZero() {
		f.To = time.Now().UTC()
	}
	if f.From.IsZero() {
		f.From = f.To.Add(-time.Hour)
	}
	if f.StatsOrderBy == "" {
		f.StatsOrderBy = "total"
	}
}

// SortStats는 요청된 기준으로 통계를 정렬한다.
//
// 기본값이 total(총 소요 시간)인 이유: 평균이 느린 쿼리보다
// "자주 실행되어 전체 시간을 많이 먹는 쿼리"가 개선 효과가 크다.
func SortStats(stats []QueryStat, orderBy string) {
	switch orderBy {
	case "mean":
		sort.SliceStable(stats, func(i, j int) bool { return stats[i].MeanMs > stats[j].MeanMs })
	case "calls":
		sort.SliceStable(stats, func(i, j int) bool { return stats[i].Calls > stats[j].Calls })
	case "max":
		sort.SliceStable(stats, func(i, j int) bool { return stats[i].MaxMs > stats[j].MaxMs })
	default:
		sort.SliceStable(stats, func(i, j int) bool { return stats[i].TotalMs > stats[j].TotalMs })
	}
}

// ValidStatsOrder는 정렬 기준이 유효한지 확인한다.
func ValidStatsOrder(orderBy string) bool {
	switch orderBy {
	case "", "total", "mean", "calls", "max":
		return true
	}
	return false
}

// SourceLabels는 소스 종류의 한국어 라벨이다.
var SourceLabels = map[SourceKind]string{
	SourceSlowQuery:  "슬로우 쿼리 로그",
	SourceErrorLog:   "서버 로그",
	SourceProfiler:   "프로파일러",
	SourceSlowLog:    "SLOWLOG",
	SourceStatements: "누적 쿼리 통계",
	SourceCurrent:    "실행 중인 쿼리",
}

func Label(kind SourceKind) string {
	if l, ok := SourceLabels[kind]; ok {
		return l
	}
	return string(kind)
}

// TruncateQuery는 화면과 로그에 넣을 쿼리를 적당한 길이로 자른다.
// 매우 긴 쿼리(수십 KB)가 응답을 채우는 것을 막는다.
func TruncateQuery(q string, max int) string {
	q = strings.TrimSpace(q)
	if max <= 0 {
		max = 4000
	}
	if len(q) <= max {
		return q
	}
	return q[:max] + " …(잘림)"
}
