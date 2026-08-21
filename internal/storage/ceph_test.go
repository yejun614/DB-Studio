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

// Ceph 대시보드 API를 흉내 낸 서버. 응답 모양은 Ceph 17/18의 것을 따랐다.

type fakeCeph struct {
	*httptest.Server
	logins  int
	status  string
	upOSDs  int
	token   string
	strict  bool // true면 v1.0 Accept가 아닌 요청을 415로 거절한다(버전 협상 확인)
	expired bool // true면 첫 요청에 401을 돌려준다(토큰 만료 재로그인 확인)
}

func newFakeCeph(t *testing.T) *fakeCeph {
	t.Helper()
	f := &fakeCeph{status: "HEALTH_OK", upOSDs: 6, token: "jwt-token-1"}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/auth", func(w http.ResponseWriter, r *http.Request) {
		if f.strict && r.Header.Get("Accept") != "application/vnd.ceph.api.v1.0+json" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		var body struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Username != "admin" || body.Password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"detail": "Invalid credentials"})
			return
		}
		f.logins++
		_ = json.NewEncoder(w).Encode(map[string]any{"token": f.token, "username": "admin"})
	})

	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if f.expired {
			f.expired = false
			f.token = "jwt-token-2" // 다음 로그인은 새 토큰을 준다
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"detail": "Token expired"})
			return false
		}
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"detail": "Missing token"})
			return false
		}
		return true
	}

	mux.HandleFunc("/api/health/minimal", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		checks := []any{}
		if f.status != "HEALTH_OK" {
			checks = append(checks, map[string]any{
				"type": "OSD_DOWN", "severity": "HEALTH_WARN",
				"summary": map[string]any{"message": "1 osds down"},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"health": map[string]any{"status": f.status, "checks": checks},
			"df": map[string]any{"stats": map[string]any{
				"total_bytes":          int64(10995116277760),
				"total_used_raw_bytes": int64(4398046511104),
				"total_avail_bytes":    int64(6597069766656),
			}},
			"osd_map": map[string]any{"osd_map": map[string]any{
				"num_osds": 6, "num_up_osds": f.upOSDs, "num_in_osds": 6,
			}},
			"pg_info": map[string]any{"statuses": map[string]any{
				"active+clean": 320, "active+undersized+degraded": 12,
			}},
			"pools":   3,
			"hosts":   3,
			"rgw":     1,
			"mgr_map": map[string]any{"active_name": "ceph-mgr-a"},
		})
	})

	mux.HandleFunc("/api/pool", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode([]any{
			map[string]any{
				"pool_name": "rbd", "pool": 1, "type": "replicated", "size": 3,
				"min_size": 2, "pg_num": 128,
				"application_metadata": map[string]any{"rbd": map[string]any{}},
				"stats": map[string]any{
					"bytes_used": map[string]any{"latest": 1073741824},
					"objects":    map[string]any{"latest": 512},
				},
				"df_stats": map[string]any{"max_avail": 2147483648},
			},
		})
	})

	mux.HandleFunc("/api/osd", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode([]any{
			map[string]any{
				"osd": 0, "up": 1, "in": 1, "weight": 1.0,
				"host":      map[string]any{"name": "ceph-node-1"},
				"state":     []string{"exists", "up"},
				"osd_stats": map[string]any{"stat_bytes": 1099511627776.0, "stat_bytes_used": 439804651110.0},
			},
			map[string]any{
				"osd": 1, "up": 0, "in": 1, "weight": 1.0,
				"host":      map[string]any{"name": "ceph-node-2"},
				"state":     []string{"exists"},
				"osd_stats": map[string]any{"stat_bytes": 1099511627776.0, "stat_bytes_used": 219902325555.0},
			},
		})
	})

	mux.HandleFunc("/api/rgw/bucket", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode([]string{"backups", "logs"})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func cephFor(t *testing.T, f *fakeCeph, user, pass string) *Ceph {
	t.Helper()
	u, _ := url.Parse(f.URL)
	port := 0
	if _, err := fmtSscan(u.Port(), &port); err != nil {
		t.Fatalf("port: %v", err)
	}
	return NewCeph(Config{
		Scheme: "http", Host: u.Hostname(), Port: port,
		User: user, Password: pass, Extra: map[string]string{"scheme": "http"},
	})
}

func TestCephOverview(t *testing.T) {
	f := newFakeCeph(t)
	c := cephFor(t, f, "admin", "secret")

	ov, err := c.Overview(context.Background())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov.Health.Level != HealthOK {
		t.Errorf("상태 %q, 기대 ok", ov.Health.Level)
	}
	if ov.Capacity.Total != 10995116277760 {
		t.Errorf("용량 %+v", ov.Capacity)
	}
	if got := ov.Capacity.UsedPercent(); got < 39.9 || got > 40.1 {
		t.Errorf("사용률 %.2f, 기대 40", got)
	}
	if !hasFact(ov.Facts, "OSD", "6개 중 6개 up") {
		t.Errorf("OSD 요약이 없습니다: %+v", ov.Facts)
	}
	// active+clean이 아닌 PG만 보여준다. 정상 PG 수는 보고 할 수 있는 일이 없다.
	if hasFactLabel(ov.Facts, "PG active+clean") {
		t.Error("정상 PG가 목록에 있습니다")
	}
	if !hasFactLabel(ov.Facts, "PG undersized+degraded") {
		t.Errorf("비정상 PG가 빠졌습니다: %+v", ov.Facts)
	}
}

// TestCephHealthLevels는 HEALTH_* 를 등급으로 번역하는지 본다.
func TestCephHealthLevels(t *testing.T) {
	f := newFakeCeph(t)
	c := cephFor(t, f, "admin", "secret")
	ctx := context.Background()

	f.status, f.upOSDs = "HEALTH_WARN", 5
	ov, err := c.Overview(ctx)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov.Health.Level != HealthWarn {
		t.Errorf("상태 %q, 기대 warn", ov.Health.Level)
	}
	if len(ov.Health.Checks) == 0 || !strings.Contains(ov.Health.Checks[0], "osds down") {
		t.Errorf("체크 내용이 비어 있습니다: %+v", ov.Health.Checks)
	}
	// down된 OSD는 색으로도 드러나야 한다.
	for _, fact := range ov.Facts {
		if fact.Label == "OSD" && fact.Level != "error" {
			t.Errorf("OSD가 빠졌는데 강조되지 않았습니다: %+v", fact)
		}
	}

	f.status = "HEALTH_ERR"
	ov, _ = c.Overview(ctx)
	if ov.Health.Level != HealthError || ov.Health.Score() != 2 {
		t.Errorf("HEALTH_ERR 번역 %+v", ov.Health)
	}
}

func TestCephPoolsAndOSDs(t *testing.T) {
	f := newFakeCeph(t)
	c := cephFor(t, f, "admin", "secret")
	ctx := context.Background()

	pools, err := c.Pools(ctx)
	if err != nil {
		t.Fatalf("pools: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("풀 %d개", len(pools))
	}
	p := pools[0]
	if p.Name != "rbd" || p.Size != 3 || p.Used != 1073741824 || p.Objects != 512 || p.App != "rbd" {
		t.Errorf("풀 %+v", p)
	}

	osds, err := c.OSDs(ctx)
	if err != nil {
		t.Fatalf("osds: %v", err)
	}
	if len(osds) != 2 {
		t.Fatalf("OSD %d개", len(osds))
	}
	if !osds[0].Up || osds[0].Host != "ceph-node-1" {
		t.Errorf("첫 OSD %+v", osds[0])
	}
	if osds[1].Up || !osds[1].In {
		t.Errorf("두 번째 OSD는 down·in 이어야 합니다: %+v", osds[1])
	}

	buckets, note, err := c.Buckets(ctx)
	if err != nil {
		t.Fatalf("buckets: %v", err)
	}
	if note != "" || len(buckets) != 2 || buckets[0].Name != "backups" {
		t.Errorf("버킷 %+v (%s)", buckets, note)
	}
}

// TestCephTokenReuse는 로그인을 매번 하지 않는지 본다.
//
// 매번 로그인하면 화면 한 장에 로그인이 서너 번 쌓이고, 대시보드의 감사 로그가
// 우리 앱의 로그인으로 가득 찬다.
func TestCephTokenReuse(t *testing.T) {
	f := newFakeCeph(t)
	c := cephFor(t, f, "admin", "secret")
	ctx := context.Background()

	for range 3 {
		if _, err := c.Overview(ctx); err != nil {
			t.Fatalf("overview: %v", err)
		}
	}
	if f.logins != 1 {
		t.Errorf("로그인 %d회, 기대 1회", f.logins)
	}
}

// TestCephTokenExpiry는 만료된 토큰에서 한 번 다시 로그인하는지 본다.
func TestCephTokenExpiry(t *testing.T) {
	f := newFakeCeph(t)
	c := cephFor(t, f, "admin", "secret")
	ctx := context.Background()

	if _, err := c.Overview(ctx); err != nil {
		t.Fatalf("첫 조회: %v", err)
	}
	f.expired = true
	if _, err := c.Overview(ctx); err != nil {
		t.Fatalf("만료 뒤 조회가 실패했습니다: %v", err)
	}
	if f.logins != 2 {
		t.Errorf("로그인 %d회, 기대 2회(만료 후 재로그인)", f.logins)
	}
}

// TestCephAcceptNegotiation은 버전 협상을 확인한다.
func TestCephAcceptNegotiation(t *testing.T) {
	f := newFakeCeph(t)
	f.strict = true
	c := cephFor(t, f, "admin", "secret")

	if _, err := c.Overview(context.Background()); err != nil {
		t.Fatalf("v1.0만 받는 서버에서 실패했습니다: %v", err)
	}
}

// TestCephAuthErrors는 인증 실패의 안내를 확인한다.
func TestCephAuthErrors(t *testing.T) {
	f := newFakeCeph(t)

	_, err := cephFor(t, f, "admin", "wrong").Overview(context.Background())
	if err == nil || !strings.Contains(err.Error(), "로그인 실패") {
		t.Errorf("잘못된 비밀번호 오류: %v", err)
	}

	_, err = cephFor(t, f, "", "").Overview(context.Background())
	if err == nil || !strings.Contains(err.Error(), "계정이 필요합니다") {
		t.Errorf("계정 없음 오류: %v", err)
	}
}

func TestCephCollect(t *testing.T) {
	f := newFakeCeph(t)
	f.status, f.upOSDs = "HEALTH_WARN", 5
	c := cephFor(t, f, "admin", "secret")

	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if m.OSDsTotal != 6 || m.OSDsUp != 5 || m.OSDsIn != 6 {
		t.Errorf("OSD 지표 %+v", m)
	}
	if m.PGsBad != 12 {
		t.Errorf("비정상 PG %v, 기대 12", m.PGsBad)
	}
	if m.Health.Score() != 1 {
		t.Errorf("상태 점수 %v, 기대 1", m.Health.Score())
	}
	if m.Pools != 3 {
		t.Errorf("풀 수 %v", m.Pools)
	}
}

func hasFactLabel(facts []Fact, label string) bool {
	for _, f := range facts {
		if f.Label == label {
			return true
		}
	}
	return false
}
