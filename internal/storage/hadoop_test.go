package storage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 하둡 클러스터를 세울 수는 없으므로, 실제 응답 모양을 그대로 흉내 낸 서버로 검증한다.
// 값은 하둡 3.x의 JMX·WebHDFS·YARN 응답에서 가져온 형태다.

type fakeHadoop struct {
	*httptest.Server
	requests []string // 받은 요청(경로+질의). 인증 파라미터 검증에 쓴다.
	safemode string
	missing  int64
	dead     int64
}

func newFakeHadoop(t *testing.T) *fakeHadoop {
	t.Helper()
	f := &fakeHadoop{}
	mux := http.NewServeMux()

	mux.HandleFunc("/jmx", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.String())
		qry := r.URL.Query().Get("qry")
		var bean map[string]any
		switch {
		case strings.Contains(qry, "NameNodeInfo"):
			bean = map[string]any{
				"Version":  "3.3.6, r1be78238728da9266a4f88195058f08fd012bf9c",
				"Total":    float64(21474836480),
				"Used":     float64(5368709120),
				"Free":     float64(16106127360),
				"Safemode": f.safemode,
			}
		case strings.Contains(qry, "FSNamesystemState"):
			bean = map[string]any{
				"NumLiveDataNodes": float64(3),
				"NumDeadDataNodes": float64(f.dead),
				// 문자열로 오는 버전이 있어 일부러 섞어 둔다.
				"VolumeFailuresTotal": "0",
			}
		case strings.Contains(qry, "name=FSNamesystem"):
			bean = map[string]any{
				"MissingBlocks":         float64(f.missing),
				"UnderReplicatedBlocks": float64(2),
				"CorruptBlocks":         float64(0),
				"BlocksTotal":           float64(1024),
			}
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"beans": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beans": []any{bean}})
	})

	mux.HandleFunc("/webhdfs/v1/", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.String())
		op := r.URL.Query().Get("op")
		switch op {
		case "LISTSTATUS":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"FileStatuses": map[string]any{"FileStatus": []any{
					map[string]any{
						"pathSuffix": "warehouse", "type": "DIRECTORY", "length": 0,
						"owner": "hive", "group": "hadoop", "permission": "755",
						"modificationTime": 1723000000000,
					},
					map[string]any{
						"pathSuffix": "part-00000.parquet", "type": "FILE", "length": 1048576,
						"owner": "spark", "group": "hadoop", "permission": "644",
						"replication": 3, "modificationTime": 1723000500000,
					},
				}},
			})
		case "GETCONTENTSUMMARY":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ContentSummary": map[string]any{
					"directoryCount": 12, "fileCount": 340, "length": 987654321,
					"quota": -1, "spaceConsumed": 2962962963, "spaceQuota": -1,
				},
			})
		case "MKDIRS", "RENAME":
			_ = json.NewEncoder(w).Encode(map[string]any{"boolean": true})
		case "DELETE":
			if strings.Contains(r.URL.Path, "protected") {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"RemoteException": map[string]any{
						"exception": "AccessControlException",
						"message":   "Permission denied: user=dr.who, access=WRITE",
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"boolean": true})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	mux.HandleFunc("/ws/v1/cluster/metrics", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"clusterMetrics": map[string]any{
			"appsRunning": 4, "appsPending": 1, "allocatedMB": 40960, "totalMB": 131072,
			"activeNodes": 3, "lostNodes": 0, "unhealthyNodes": 1,
		}})
	})
	mux.HandleFunc("/ws/v1/cluster/apps", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.String())
		_ = json.NewEncoder(w).Encode(map[string]any{"apps": map[string]any{"app": []any{
			map[string]any{
				"id": "application_1723_0001", "name": "nightly-etl", "user": "spark",
				"queue": "default", "state": "RUNNING", "progress": 42.5,
				"startedTime": 1723000000000,
			},
		}}})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func hadoopFor(t *testing.T, f *fakeHadoop, extra map[string]string) *Hadoop {
	t.Helper()
	u, err := url.Parse(f.URL)
	if err != nil {
		t.Fatalf("url: %v", err)
	}
	port := 0
	if _, err := fmtSscan(u.Port(), &port); err != nil {
		t.Fatalf("port: %v", err)
	}
	cfg := Config{Scheme: "http", Host: u.Hostname(), Port: port, User: "hdfs", Extra: map[string]string{}}
	for k, v := range extra {
		cfg.Extra[k] = v
	}
	return NewHadoop(cfg)
}

func TestHadoopOverview(t *testing.T) {
	f := newFakeHadoop(t)
	h := hadoopFor(t, f, map[string]string{"yarn_url": f.URL})

	ov, err := h.Overview(context.Background())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if !strings.HasPrefix(ov.Version, "3.3.6") {
		t.Errorf("버전 %q", ov.Version)
	}
	if ov.Capacity.Total != 21474836480 || ov.Capacity.Used != 5368709120 {
		t.Errorf("용량 %+v", ov.Capacity)
	}
	if got := ov.Capacity.UsedPercent(); got < 24.9 || got > 25.1 {
		t.Errorf("사용률 %.2f, 기대 25", got)
	}
	// 복제 부족 블록이 있으므로 정상이 아니라 주의여야 한다.
	if ov.Health.Level != HealthWarn {
		t.Errorf("상태 %q, 기대 warn (%s)", ov.Health.Level, ov.Health.Summary)
	}
	if !hasFact(ov.Facts, "YARN 실행 앱", "4개") {
		t.Errorf("YARN 지표가 개요에 없습니다: %+v", ov.Facts)
	}
}

// TestHadoopHealthLevels는 상태 등급의 우선순위를 고정한다.
//
// 이 순서가 중요한 이유: 손실 블록은 데이터가 이미 없다는 뜻이고, 세이프모드는 아직
// 아무것도 잃지 않았다는 뜻이다. 둘이 동시에 있을 때 "세이프모드"만 보여주면 사람은
// 기다리면 된다고 판단한다.
func TestHadoopHealthLevels(t *testing.T) {
	f := newFakeHadoop(t)
	h := hadoopFor(t, f, nil)
	ctx := context.Background()

	f.missing, f.safemode, f.dead = 0, "", 0
	// 복제 부족(2)이 남아 있으므로 주의다.
	if ov, _ := h.Overview(ctx); ov.Health.Level != HealthWarn {
		t.Errorf("기본 상태 %q, 기대 warn", ov.Health.Level)
	}

	f.safemode = "Safe mode is ON. The reported blocks 0 needs additional 5 blocks"
	if ov, _ := h.Overview(ctx); ov.Health.Level != HealthWarn ||
		!strings.Contains(ov.Health.Summary, "세이프모드") {
		t.Errorf("세이프모드 상태 %+v", ov.Health)
	}

	f.missing = 3
	ov, _ := h.Overview(ctx)
	if ov.Health.Level != HealthError {
		t.Errorf("손실 블록이 있는데 상태가 %q입니다", ov.Health.Level)
	}
	if !strings.Contains(ov.Health.Summary, "손실") {
		t.Errorf("요약에 손실이 없습니다: %q", ov.Health.Summary)
	}
}

func TestHadoopBrowse(t *testing.T) {
	f := newFakeHadoop(t)
	h := hadoopFor(t, f, nil)

	entries, err := h.List(context.Background(), "/user/hive")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("항목 %d개, 기대 2개", len(entries))
	}
	if !entries[0].Dir || entries[0].Name != "warehouse" {
		t.Errorf("첫 항목 %+v", entries[0])
	}
	if entries[1].Size != 1048576 || entries[1].Replication != 3 {
		t.Errorf("파일 항목 %+v", entries[1])
	}
	if entries[1].ModifiedAt.IsZero() {
		t.Error("수정 시각이 비어 있습니다")
	}

	// 계정이 user.name으로 실려야 한다. 빠지면 서버가 기본 사용자로 처리해
	// "권한 없음"이 나는데, 그 원인을 화면에서 짐작할 수 없다.
	last := f.requests[len(f.requests)-1]
	if !strings.Contains(last, "user.name=hdfs") {
		t.Errorf("요청에 계정이 없습니다: %s", last)
	}

	sum, err := h.Summary(context.Background(), "/user/hive")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Files != 340 || sum.Directories != 12 || sum.Quota != -1 {
		t.Errorf("요약 %+v", sum)
	}
}

// TestHadoopPathEncoding은 공백·한글이 든 경로가 그대로 전달되는지 본다.
func TestHadoopPathEncoding(t *testing.T) {
	f := newFakeHadoop(t)
	h := hadoopFor(t, f, nil)

	if _, err := h.List(context.Background(), "/data/판매 실적/2026"); err != nil {
		t.Fatalf("list: %v", err)
	}
	last := f.requests[len(f.requests)-1]
	// 구분자(/)는 살아 있어야 하고 이름만 인코딩돼야 한다.
	if !strings.HasPrefix(last, "/webhdfs/v1/data/") || strings.Contains(last, "%2F") {
		t.Errorf("경로 인코딩이 잘못됐습니다: %s", last)
	}
}

func TestHadoopMutations(t *testing.T) {
	f := newFakeHadoop(t)
	h := hadoopFor(t, f, nil)
	ctx := context.Background()

	if err := h.Mkdir(ctx, "/tmp/new-dir"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := h.Rename(ctx, "/tmp/new-dir", "/tmp/renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := h.Delete(ctx, "/tmp/renamed", true); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// 루트는 어떤 경우에도 지우지 않는다.
	if err := h.Delete(ctx, "/", true); err == nil {
		t.Error("루트 삭제가 통과했습니다")
	}
	if err := h.Mkdir(ctx, "/"); err == nil {
		t.Error("루트 생성이 통과했습니다")
	}
}

// TestHadoopErrorMessage는 하둡의 예외를 사람이 읽을 말로 바꾸는지 본다.
func TestHadoopErrorMessage(t *testing.T) {
	f := newFakeHadoop(t)
	h := hadoopFor(t, f, nil)

	err := h.Delete(context.Background(), "/protected/data", true)
	if err == nil {
		t.Fatal("권한 오류가 통과했습니다")
	}
	msg := err.Error()
	if !strings.Contains(msg, "HDFS 권한이 없습니다") {
		t.Errorf("메시지가 원인을 설명하지 않습니다: %s", msg)
	}
	// 원문도 남아야 조사할 수 있다.
	if !strings.Contains(msg, "Permission denied") {
		t.Errorf("원문이 사라졌습니다: %s", msg)
	}
}

func TestHadoopApps(t *testing.T) {
	f := newFakeHadoop(t)

	// YARN 주소가 없으면 그 사실을 분명히 말해야 한다.
	if _, err := hadoopFor(t, f, nil).Apps(context.Background(), "RUNNING", 10); err == nil ||
		!strings.Contains(err.Error(), "yarn_url") {
		t.Errorf("YARN 미설정 오류: %v", err)
	}

	h := hadoopFor(t, f, map[string]string{"yarn_url": f.URL})
	apps, err := h.Apps(context.Background(), "RUNNING", 10)
	if err != nil {
		t.Fatalf("apps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "nightly-etl" || apps[0].State != "RUNNING" {
		t.Fatalf("앱 목록 %+v", apps)
	}
	if apps[0].StartAt.IsZero() {
		t.Error("시작 시각이 비어 있습니다")
	}
}

func TestHadoopCollect(t *testing.T) {
	f := newFakeHadoop(t)
	f.dead = 1
	h := hadoopFor(t, f, map[string]string{"yarn_url": f.URL})

	m, err := h.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if m.LiveNodes != 3 || m.DeadNodes != 1 {
		t.Errorf("노드 수 live=%v dead=%v", m.LiveNodes, m.DeadNodes)
	}
	if m.UnderRepBlocks != 2 {
		t.Errorf("복제 부족 %v", m.UnderRepBlocks)
	}
	if m.YARN == nil || m.YARN.AppsRunning != 4 {
		t.Errorf("YARN 지표 %+v", m.YARN)
	}
	if m.Health.Score() != 1 {
		t.Errorf("상태 점수 %v, 기대 1(주의)", m.Health.Score())
	}
}

func TestCleanPath(t *testing.T) {
	cases := map[string]string{
		"":              "/",
		"/":             "/",
		"user/hive":     "/user/hive",
		"/user//hive/":  "/user/hive",
		"/user/../etc":  "/etc",
		"/user/hive/..": "/user",
	}
	for in, want := range cases {
		if got := CleanPath(in); got != want {
			t.Errorf("CleanPath(%q) = %q, 기대 %q", in, got, want)
		}
	}
}

func hasFact(facts []Fact, label, value string) bool {
	for _, f := range facts {
		if f.Label == label && f.Value == value {
			return true
		}
	}
	return false
}

// fmtSscan은 테스트에서 포트 문자열을 숫자로 바꾼다(작은 헬퍼).
func fmtSscan(s string, out *int) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errBadPort
		}
		n = n*10 + int(r-'0')
	}
	*out = n
	return 1, nil
}

var errBadPort = errBadPortType{}

type errBadPortType struct{}

func (errBadPortType) Error() string { return "포트가 숫자가 아닙니다" }
