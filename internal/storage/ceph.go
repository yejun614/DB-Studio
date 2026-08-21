package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"dbstudio/internal/opsapi"
)

// Ceph 클라이언트.
//
// Ceph Manager의 대시보드 REST API를 쓴다(기본 8443/https). 이유는 하둡과 같다:
// 네이티브 프로토콜(librados)은 C 라이브러리를 뜻하고, 그것은 단일 바이너리 원칙과
// 충돌한다. 대시보드 API는 클러스터 상태·풀·OSD·버킷을 모두 돌려준다.
//
// 인증은 토큰이다. POST /api/auth 로 받은 JWT를 이후 요청의 Authorization에 싣고,
// 만료되면 다시 받는다. 토큰을 캐시하는 이유: 화면 한 장이 서너 번의 호출로 이뤄지는데
// 매번 로그인하면 그만큼 대시보드에 인증 부하를 주고, 감사 로그가 로그인으로 가득 찬다.
//
// **읽기 전용이다.** 풀 삭제·OSD out·CRUSH 변경 같은 조작은 되돌릴 수 없고 클러스터
// 전체의 데이터 배치를 바꾼다. 그 판단을 이 앱의 버튼 하나로 옮겨 놓는 것은 위험을
// 옮기는 것일 뿐이라, 상태를 **보여주는** 것까지만 한다.

// CephDefaultPort는 대시보드 포트다.
const CephDefaultPort = 8443

// Ceph는 Ceph 클러스터 클라이언트다.
type Ceph struct {
	cfg    Config
	client *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func NewCeph(cfg Config) *Ceph {
	if cfg.Scheme == "" || cfg.Scheme == "http" {
		// 대시보드는 기본이 HTTPS다. http로 두면 첫 호출이 TLS 오류로 끝나는데,
		// 그 메시지는 "잘못된 프로토콜"이라 원인을 짐작하기 어렵다.
		if _, ok := cfg.Extra["scheme"]; !ok {
			cfg.Scheme = "https"
		}
	}
	return &Ceph{cfg: cfg, client: cfg.HTTPClient()}
}

func (c *Ceph) Kind() string { return KindCeph }

// acceptTypes는 대시보드 API의 버전 협상 값이다.
//
// 여러 개를 두는 이유: 엔드포인트마다 버전이 다르고(예: /api/rgw/bucket 은 v1.1),
// 맞지 않으면 415를 돌려준다. 하나씩 시도하면 Ceph 버전이 달라도 동작한다.
var acceptTypes = []string{
	"application/vnd.ceph.api.v1.0+json",
	"application/vnd.ceph.api.v1.1+json",
	"application/vnd.ceph.api.v2.0+json",
	"application/json",
}

func (c *Ceph) login(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && time.Now().Before(c.expires) {
		tok := c.token
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	if strings.TrimSpace(c.cfg.User) == "" {
		return "", errors.New("Ceph 대시보드 계정이 필요합니다 (커넥션의 계정·비밀번호)")
	}
	body, _ := json.Marshal(map[string]string{
		"username": c.cfg.User, "password": c.cfg.Password,
	})
	var out struct {
		Token       string `json:"token"`
		Username    string `json:"username"`
		Permissions any    `json:"permissions"`
	}
	var lastErr error
	for _, accept := range acceptTypes {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.cfg.BaseURL()+"/api/auth", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", accept)
		req.Header.Set("Accept", accept)
		err = opsapi.DoJSON(ctx, c.client, req, &out)
		if err == nil && out.Token != "" {
			c.mu.Lock()
			c.token = out.Token
			// 대시보드 토큰의 기본 수명은 8시간이지만 값을 돌려주지 않는다.
			// 짧게 잡아 두고 만료되면 다시 받는다 — 만료된 토큰으로 계속 401을 받는 것보다 낫다.
			c.expires = time.Now().Add(30 * time.Minute)
			c.mu.Unlock()
			return out.Token, nil
		}
		lastErr = err
		var he *opsapi.HTTPError
		if asHTTPError(err, &he) && he.Status == http.StatusUnsupportedMediaType {
			continue // 버전이 맞지 않는다. 다음 값으로 시도한다.
		}
		break
	}
	if lastErr == nil {
		lastErr = errors.New("토큰을 받지 못했습니다")
	}
	return "", fmt.Errorf("Ceph 대시보드 로그인 실패: %w", lastErr)
}

// get은 대시보드 API를 부른다. 401이면 한 번 다시 로그인하고 재시도한다.
func (c *Ceph) get(ctx context.Context, path string, query url.Values, out any) error {
	for attempt := range 2 {
		token, err := c.login(ctx)
		if err != nil {
			return err
		}
		var lastErr error
		for _, accept := range acceptTypes {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				opsapi.JoinURL(c.cfg.BaseURL(), path, query), nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Accept", accept)
			err = opsapi.DoJSON(ctx, c.client, req, out)
			if err == nil {
				return nil
			}
			lastErr = err
			var he *opsapi.HTTPError
			if asHTTPError(err, &he) {
				if he.Status == http.StatusUnsupportedMediaType {
					continue
				}
				if he.Status == http.StatusUnauthorized && attempt == 0 {
					// 토큰이 만료됐다. 버리고 바깥 루프에서 다시 로그인한다.
					c.mu.Lock()
					c.token, c.expires = "", time.Time{}
					c.mu.Unlock()
				}
			}
			break
		}
		var he *opsapi.HTTPError
		if asHTTPError(lastErr, &he) && he.Status == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		return cephError(lastErr)
	}
	return errors.New("Ceph 대시보드 인증에 계속 실패했습니다")
}

// Ping은 접속과 인증을 확인한다.
func (c *Ceph) Ping(ctx context.Context) (string, error) {
	var out struct {
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		Versions  map[string]any `json:"versions"`
		MonStatus struct {
			MonMap struct {
				Mons []struct {
					Name string `json:"name"`
				} `json:"mons"`
			} `json:"monmap"`
		} `json:"mon_status"`
	}
	if err := c.get(ctx, "/api/health/minimal", nil, &out); err != nil {
		return "", err
	}
	return out.Health.Status, nil
}

// healthMinimal은 대시보드의 요약 응답이다.
//
// 필드를 넉넉히 두고 전부 optional로 다루는 이유: Ceph 버전마다 이 응답의 모양이 조금씩
// 다르다. 하나라도 없으면 실패로 처리하면, 버전이 바뀔 때마다 화면 전체가 빈다.
type healthMinimal struct {
	Health struct {
		Status string `json:"status"`
		Checks []struct {
			Type     string `json:"type"`
			Severity string `json:"severity"`
			Summary  struct {
				Message string `json:"message"`
			} `json:"summary"`
		} `json:"checks"`
	} `json:"health"`
	DF struct {
		Stats struct {
			TotalBytes         int64 `json:"total_bytes"`
			TotalUsedRawBytes  int64 `json:"total_used_raw_bytes"`
			TotalAvailBytes    int64 `json:"total_avail_bytes"`
			NumOSDs            int64 `json:"num_osds"`
			NumPerPoolOmapOSDs int64 `json:"num_per_pool_omap_osds"`
		} `json:"stats"`
	} `json:"df"`
	OSDMap struct {
		OSDMap struct {
			NumOSDs   int64 `json:"num_osds"`
			NumUpOSDs int64 `json:"num_up_osds"`
			NumInOSDs int64 `json:"num_in_osds"`
		} `json:"osd_map"`
	} `json:"osd_map"`
	PGInfo struct {
		Statuses  map[string]int64 `json:"statuses"`
		PGsPerOSD float64          `json:"pgs_per_osd"`
	} `json:"pg_info"`
	Pools  int64 `json:"pools"`
	Hosts  int64 `json:"hosts"`
	RGW    int64 `json:"rgw"`
	MgrMap struct {
		ActiveName string `json:"active_name"`
	} `json:"mgr_map"`
}

// Overview는 클러스터 개요다.
func (c *Ceph) Overview(ctx context.Context) (*Overview, error) {
	var hm healthMinimal
	if err := c.get(ctx, "/api/health/minimal", nil, &hm); err != nil {
		return nil, err
	}
	ov := &Overview{Kind: KindCeph}
	ov.Capacity = Capacity{
		Total:     hm.DF.Stats.TotalBytes,
		Used:      hm.DF.Stats.TotalUsedRawBytes,
		Available: hm.DF.Stats.TotalAvailBytes,
	}

	checks := make([]string, 0, len(hm.Health.Checks))
	for _, ck := range hm.Health.Checks {
		msg := strings.TrimSpace(ck.Summary.Message)
		if msg == "" {
			msg = ck.Type
		}
		checks = append(checks, msg)
	}
	ov.Health = Health{Summary: hm.Health.Status, Checks: checks}
	switch hm.Health.Status {
	case "HEALTH_OK":
		ov.Health.Level, ov.Health.Summary = HealthOK, "정상 (HEALTH_OK)"
	case "HEALTH_WARN":
		ov.Health.Level, ov.Health.Summary = HealthWarn, "주의 (HEALTH_WARN)"
	case "HEALTH_ERR":
		ov.Health.Level, ov.Health.Summary = HealthError, "위험 (HEALTH_ERR)"
	default:
		ov.Health.Level = HealthUnknown
		if ov.Health.Summary == "" {
			ov.Health.Summary = "상태를 알 수 없습니다"
		}
	}

	om := hm.OSDMap.OSDMap
	down := om.NumOSDs - om.NumUpOSDs
	out := om.NumOSDs - om.NumInOSDs
	ov.Facts = []Fact{
		{Label: "OSD", Value: fmt.Sprintf("%d개 중 %d개 up", om.NumOSDs, om.NumUpOSDs),
			Level: opsapi.LevelIf(down > 0, "error")},
		{Label: "OSD in", Value: fmt.Sprintf("%d개", om.NumInOSDs), Level: opsapi.LevelIf(out > 0, "warn")},
		{Label: "호스트", Value: fmt.Sprintf("%d대", hm.Hosts)},
		{Label: "풀", Value: fmt.Sprintf("%d개", hm.Pools)},
	}
	if hm.MgrMap.ActiveName != "" {
		ov.Facts = append(ov.Facts, Fact{Label: "활성 매니저", Value: hm.MgrMap.ActiveName})
	}
	if hm.RGW > 0 {
		ov.Facts = append(ov.Facts, Fact{Label: "RGW 데몬", Value: fmt.Sprintf("%d개", hm.RGW)})
	}
	// PG 상태는 "clean이 아닌 것"만 보여준다. 정상 PG 수는 클러스터 크기에 비례해
	// 커지기만 하고, 그 숫자를 보고 할 수 있는 일이 없다.
	for status, n := range hm.PGInfo.Statuses {
		if n == 0 || strings.Contains(status, "active+clean") {
			continue
		}
		// 앞의 "active+"는 어느 상태에나 붙어 있어 구분에 쓸모가 없다. 떼어 내면
		// 정작 봐야 할 부분(degraded, recovering)이 라벨 앞으로 온다.
		ov.Facts = append(ov.Facts, Fact{
			Label: "PG " + strings.TrimPrefix(status, "active+"), Value: fmt.Sprintf("%d개", n),
			Level: opsapi.LevelIf(strings.Contains(status, "degraded") ||
				strings.Contains(status, "inconsistent") || strings.Contains(status, "down"), "warn"),
		})
	}
	return ov, nil
}

// Pools는 풀 목록이다.
func (c *Ceph) Pools(ctx context.Context) ([]Pool, error) {
	var raw []struct {
		PoolName      string         `json:"pool_name"`
		Pool          int            `json:"pool"`
		Type          string         `json:"type"`
		Size          int            `json:"size"`
		MinSize       int            `json:"min_size"`
		PGNum         int            `json:"pg_num"`
		ApplicationMD map[string]any `json:"application_metadata"`
		Stats         struct {
			Bytes struct {
				Latest int64 `json:"latest"`
			} `json:"bytes_used"`
			Objects struct {
				Latest int64 `json:"latest"`
			} `json:"objects"`
		} `json:"stats"`
		DFStats struct {
			Stored   int64 `json:"stored"`
			MaxAvail int64 `json:"max_avail"`
			Objects  int64 `json:"objects"`
		} `json:"df_stats"`
	}
	if err := c.get(ctx, "/api/pool", url.Values{"stats": {"true"}}, &raw); err != nil {
		return nil, err
	}
	pools := make([]Pool, 0, len(raw))
	for _, p := range raw {
		pool := Pool{
			Name: p.PoolName, ID: p.Pool, Type: p.Type, Size: p.Size,
			MinSize: p.MinSize, PGNum: p.PGNum,
			Used: p.Stats.Bytes.Latest, Objects: p.Stats.Objects.Latest,
			MaxAvail: p.DFStats.MaxAvail,
		}
		if pool.Used == 0 {
			pool.Used = p.DFStats.Stored
		}
		if pool.Objects == 0 {
			pool.Objects = p.DFStats.Objects
		}
		for app := range p.ApplicationMD {
			pool.App = app
			break
		}
		pools = append(pools, pool)
	}
	return pools, nil
}

// OSDs는 OSD 목록이다.
func (c *Ceph) OSDs(ctx context.Context) ([]OSD, error) {
	var raw []struct {
		ID   int `json:"osd"`
		Up   int `json:"up"`
		In   int `json:"in"`
		Host struct {
			Name string `json:"name"`
		} `json:"host"`
		Weight float64  `json:"weight"`
		State  []string `json:"state"`
		Stats  struct {
			StatBytes     float64 `json:"stat_bytes"`
			StatBytesUsed float64 `json:"stat_bytes_used"`
		} `json:"osd_stats"`
		Tree struct {
			Status string `json:"status"`
		} `json:"tree"`
	}
	if err := c.get(ctx, "/api/osd", nil, &raw); err != nil {
		return nil, err
	}
	osds := make([]OSD, 0, len(raw))
	for _, o := range raw {
		status := strings.Join(o.State, ",")
		if status == "" {
			status = o.Tree.Status
		}
		osds = append(osds, OSD{
			ID: o.ID, Up: o.Up == 1, In: o.In == 1, Host: o.Host.Name,
			Total: int64(o.Stats.StatBytes), Used: int64(o.Stats.StatBytesUsed),
			Weight: o.Weight, Status: status,
		})
	}
	return osds, nil
}

// Buckets는 RGW 버킷 목록이다. RGW가 없으면 빈 목록과 사유를 돌려준다.
func (c *Ceph) Buckets(ctx context.Context) ([]Bucket, string, error) {
	var names []string
	err := c.get(ctx, "/api/rgw/bucket", nil, &names)
	if err != nil {
		var he *opsapi.HTTPError
		if asHTTPError(err, &he) && (he.Status == http.StatusNotFound || he.Status == http.StatusServiceUnavailable) {
			return nil, "오브젝트 게이트웨이(RGW)가 없거나 대시보드에 설정되지 않았습니다.", nil
		}
		// 이름 목록이 아니라 객체 목록을 주는 버전도 있다. 그때는 다시 읽어 본다.
		var objs []struct {
			Bucket string `json:"bucket"`
			Owner  string `json:"owner"`
		}
		if e2 := c.get(ctx, "/api/rgw/bucket", nil, &objs); e2 == nil {
			out := make([]Bucket, 0, len(objs))
			for _, b := range objs {
				out = append(out, Bucket{Name: b.Bucket, Owner: b.Owner})
			}
			return out, "", nil
		}
		return nil, "", err
	}
	out := make([]Bucket, 0, len(names))
	for _, n := range names {
		out = append(out, Bucket{Name: n})
	}
	return out, "", nil
}

// CephMetrics는 폴러가 쓰는 값이다.
type CephMetrics struct {
	Capacity  Capacity
	Health    Health
	OSDsTotal float64
	OSDsUp    float64
	OSDsIn    float64
	PGsBad    float64
	Pools     float64
}

// Collect는 지표 수집용 값이다.
func (c *Ceph) Collect(ctx context.Context) (*CephMetrics, error) {
	var hm healthMinimal
	if err := c.get(ctx, "/api/health/minimal", nil, &hm); err != nil {
		return nil, err
	}
	ov, err := c.Overview(ctx)
	if err != nil {
		return nil, err
	}
	var bad int64
	for status, n := range hm.PGInfo.Statuses {
		if !strings.Contains(status, "active+clean") {
			bad += n
		}
	}
	return &CephMetrics{
		Capacity:  ov.Capacity,
		Health:    ov.Health,
		OSDsTotal: float64(hm.OSDMap.OSDMap.NumOSDs),
		OSDsUp:    float64(hm.OSDMap.OSDMap.NumUpOSDs),
		OSDsIn:    float64(hm.OSDMap.OSDMap.NumInOSDs),
		PGsBad:    float64(bad),
		Pools:     float64(hm.Pools),
	}, nil
}

// cephError는 대시보드의 오류 본문을 사람이 읽을 말로 바꾼다.
func cephError(err error) error {
	var he *opsapi.HTTPError
	if !asHTTPError(err, &he) {
		return err
	}
	var payload struct {
		Detail string `json:"detail"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal([]byte(he.Body), &payload)
	detail := strings.TrimSpace(payload.Detail)
	if detail == "" {
		detail = opsapi.Snippet(he.Body)
	}
	switch he.Status {
	case http.StatusUnauthorized:
		return fmt.Errorf("인증이 거부됐습니다. 대시보드 계정과 비밀번호를 확인하세요: %s", detail)
	case http.StatusForbidden:
		return fmt.Errorf("이 계정에는 권한이 없습니다. 대시보드에서 읽기 권한을 확인하세요: %s", detail)
	case http.StatusNotFound:
		return fmt.Errorf("대시보드에 해당 기능이 없습니다(%d). Ceph 버전이나 모듈 설정을 확인하세요: %s",
			he.Status, detail)
	}
	return fmt.Errorf("Ceph 대시보드 오류(%d): %s", he.Status, detail)
}

// asHTTPError는 errors.As의 얇은 감싸기다(호출부를 짧게 유지한다).
func asHTTPError(err error, target **opsapi.HTTPError) bool {
	if err == nil {
		return false
	}
	return errors.As(err, target)
}
