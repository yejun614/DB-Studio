package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/opsapi"
)

// 하둡 클라이언트.
//
// 세 곳을 본다.
//   - NameNode JMX (기본 9870): 용량·데이터노드·블록 상태. 클러스터 전체의 건강이 여기 있다.
//   - WebHDFS (같은 포트): 경로 탐색과 디렉터리 조작.
//   - YARN ResourceManager (기본 8088, 옵션): 실행 중인 애플리케이션과 자원 사용률.
//
// 왜 REST만 쓰는가: HDFS RPC를 쓰려면 하둡 클라이언트 라이브러리가 필요하고, 그것은 JVM
// 또는 대형 의존성을 뜻한다. 이 앱은 단일 바이너리를 지키기로 했고, 관리 화면에 필요한
// 것(용량·목록·조작)은 전부 REST로 된다.
//
// 인증: 의사(pseudo) 인증에서는 user.name 질의 파라미터가 곧 사용자다. Knox나 프록시
// 뒤에 있으면 기본 인증(Basic)을 함께 보낸다. Kerberos(SPNEGO)는 지원하지 않는다 —
// 티켓 캐시와 keytab 관리가 필요해 이 앱의 범위를 넘는다(문서에 명시).

// HadoopDefaultPort는 NameNode HTTP 포트다(하둡 3.x).
const HadoopDefaultPort = 9870

// Hadoop은 하둡 클러스터 클라이언트다.
type Hadoop struct {
	cfg    Config
	client *http.Client
}

func NewHadoop(cfg Config) *Hadoop {
	return &Hadoop{cfg: cfg, client: cfg.HTTPClient()}
}

func (h *Hadoop) Kind() string { return KindHadoop }

// hdfsUser는 WebHDFS에 보낼 사용자 이름이다.
//
// 비어 있으면 보내지 않는다: 의사 인증에서 user.name이 없으면 서버는 기본 사용자
// (보통 dr.who)로 처리하고, 그 계정은 대개 아무 권한이 없어 목록조차 못 본다.
// 그때의 오류 메시지가 "권한 없음"이므로, 사용자가 계정을 비워 둔 것이 원인임을
// 알 수 있도록 안내에 그 사실을 적는다.
func (h *Hadoop) hdfsUser() string { return strings.TrimSpace(h.cfg.User) }

func (h *Hadoop) newRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	// 프록시(Knox 등) 뒤에 있는 경우를 위해 비밀번호가 있으면 기본 인증도 함께 보낸다.
	if h.cfg.Password != "" {
		req.SetBasicAuth(h.cfg.User, h.cfg.Password)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (h *Hadoop) query(extra url.Values) url.Values {
	q := url.Values{}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	if u := h.hdfsUser(); u != "" {
		q.Set("user.name", u)
	}
	return q
}

// jmxBean은 JMX 빈 하나를 읽는다.
func (h *Hadoop) jmxBean(ctx context.Context, qry string) (map[string]any, error) {
	u := opsapi.JoinURL(h.cfg.BaseURL(), "/jmx", url.Values{"qry": {qry}})
	req, err := h.newRequest(ctx, http.MethodGet, u)
	if err != nil {
		return nil, err
	}
	var out struct {
		Beans []map[string]any `json:"beans"`
	}
	if err := opsapi.DoJSON(ctx, h.client, req, &out); err != nil {
		return nil, err
	}
	if len(out.Beans) == 0 {
		return nil, fmt.Errorf("JMX에서 %s 를 찾지 못했습니다", qry)
	}
	return out.Beans[0], nil
}

// Ping은 접속을 확인하고 버전을 돌려준다.
func (h *Hadoop) Ping(ctx context.Context) (string, error) {
	bean, err := h.jmxBean(ctx, "Hadoop:service=NameNode,name=NameNodeInfo")
	if err != nil {
		return "", err
	}
	return str(bean["Version"]), nil
}

// Overview는 클러스터 개요다.
func (h *Hadoop) Overview(ctx context.Context) (*Overview, error) {
	ov := &Overview{Kind: KindHadoop, Health: Health{Level: HealthUnknown, Summary: "확인하지 못했습니다"}}

	info, err := h.jmxBean(ctx, "Hadoop:service=NameNode,name=NameNodeInfo")
	if err != nil {
		return nil, fmt.Errorf("NameNode 상태를 읽지 못했습니다: %w", err)
	}
	ov.Version = str(info["Version"])
	ov.Capacity = Capacity{
		Total:     num64(info["Total"]),
		Used:      num64(info["Used"]),
		Available: num64(info["Free"]),
	}

	state, err := h.jmxBean(ctx, "Hadoop:service=NameNode,name=FSNamesystemState")
	if err != nil {
		ov.Notes = append(ov.Notes, "데이터노드 수를 읽지 못했습니다: "+err.Error())
		state = map[string]any{}
	}
	fs, err := h.jmxBean(ctx, "Hadoop:service=NameNode,name=FSNamesystem")
	if err != nil {
		ov.Notes = append(ov.Notes, "블록 상태를 읽지 못했습니다: "+err.Error())
		fs = map[string]any{}
	}

	live := num64(state["NumLiveDataNodes"])
	dead := num64(state["NumDeadDataNodes"])
	missing := num64(fs["MissingBlocks"])
	under := num64(fs["UnderReplicatedBlocks"])
	corrupt := num64(fs["CorruptBlocks"])
	safemode := strings.TrimSpace(str(info["Safemode"]))

	ov.Facts = []Fact{
		{Label: "데이터노드", Value: fmt.Sprintf("%d대 정상", live)},
		{Label: "죽은 데이터노드", Value: fmt.Sprintf("%d대", dead), Level: opsapi.LevelIf(dead > 0, "warn")},
		{Label: "블록", Value: fmt.Sprintf("%d개", num64(fs["BlocksTotal"]))},
		{Label: "복제 부족 블록", Value: fmt.Sprintf("%d개", under), Level: opsapi.LevelIf(under > 0, "warn")},
		{Label: "손실 블록", Value: fmt.Sprintf("%d개", missing), Level: opsapi.LevelIf(missing > 0, "error")},
	}
	if corrupt > 0 {
		ov.Facts = append(ov.Facts, Fact{
			Label: "손상 블록", Value: fmt.Sprintf("%d개", corrupt), Level: "error"})
	}
	if safemode != "" {
		ov.Facts = append(ov.Facts, Fact{Label: "세이프모드", Value: safemode, Level: "warn"})
	}

	// 상태 등급은 "무엇이 나쁜가"의 우선순위를 그대로 따른다.
	// 손실 블록은 데이터가 이미 없다는 뜻이므로 어떤 경고보다 위다.
	switch {
	case missing > 0 || corrupt > 0:
		ov.Health = Health{Level: HealthError,
			Summary: fmt.Sprintf("손실 %d · 손상 %d 블록", missing, corrupt)}
	case safemode != "":
		ov.Health = Health{Level: HealthWarn, Summary: "세이프모드", Checks: []string{safemode}}
	case dead > 0 || under > 0:
		ov.Health = Health{Level: HealthWarn,
			Summary: fmt.Sprintf("죽은 노드 %d · 복제 부족 %d", dead, under)}
	default:
		ov.Health = Health{Level: HealthOK, Summary: "정상"}
	}

	// YARN은 선택이다. 없으면 그 사실만 적고 나머지를 보여준다.
	if base := h.yarnURL(); base != "" {
		m, err := h.yarnMetrics(ctx)
		if err != nil {
			ov.Notes = append(ov.Notes, "YARN 상태를 읽지 못했습니다: "+err.Error())
		} else {
			ov.Facts = append(ov.Facts,
				Fact{Label: "YARN 실행 앱", Value: fmt.Sprintf("%d개", m.AppsRunning)},
				Fact{Label: "YARN 대기 앱", Value: fmt.Sprintf("%d개", m.AppsPending),
					Level: opsapi.LevelIf(m.AppsPending > 0, "warn")},
				Fact{Label: "YARN 메모리", Value: fmt.Sprintf("%s / %s",
					opsapi.HumanBytes(m.AllocatedMB<<20), opsapi.HumanBytes(m.TotalMB<<20))},
			)
			if m.UnhealthyNodes > 0 || m.LostNodes > 0 {
				ov.Facts = append(ov.Facts, Fact{
					Label: "YARN 노드 이상",
					Value: fmt.Sprintf("비정상 %d · 유실 %d", m.UnhealthyNodes, m.LostNodes),
					Level: "warn"})
			}
		}
	} else {
		ov.Notes = append(ov.Notes,
			"YARN 주소(yarn_url)를 설정하면 실행 중인 애플리케이션과 자원 사용률도 함께 봅니다.")
	}
	return ov, nil
}

// yarnURL은 ResourceManager 주소다. 비어 있으면 YARN을 보지 않는다.
func (h *Hadoop) yarnURL() string {
	return strings.TrimRight(strings.TrimSpace(h.cfg.Extra["yarn_url"]), "/")
}

type yarnMetrics struct {
	AppsRunning    int64 `json:"appsRunning"`
	AppsPending    int64 `json:"appsPending"`
	AppsFailed     int64 `json:"appsFailed"`
	AllocatedMB    int64 `json:"allocatedMB"`
	TotalMB        int64 `json:"totalMB"`
	AvailableMB    int64 `json:"availableMB"`
	ActiveNodes    int64 `json:"activeNodes"`
	LostNodes      int64 `json:"lostNodes"`
	UnhealthyNodes int64 `json:"unhealthyNodes"`
}

func (h *Hadoop) yarnMetrics(ctx context.Context) (*yarnMetrics, error) {
	base := h.yarnURL()
	if base == "" {
		return nil, fmt.Errorf("YARN 주소가 설정되지 않았습니다")
	}
	req, err := h.newRequest(ctx, http.MethodGet, base+"/ws/v1/cluster/metrics")
	if err != nil {
		return nil, err
	}
	var out struct {
		ClusterMetrics yarnMetrics `json:"clusterMetrics"`
	}
	if err := opsapi.DoJSON(ctx, h.client, req, &out); err != nil {
		return nil, err
	}
	return &out.ClusterMetrics, nil
}

// Apps는 YARN 애플리케이션 목록이다.
func (h *Hadoop) Apps(ctx context.Context, states string, limit int) ([]App, error) {
	base := h.yarnURL()
	if base == "" {
		return nil, fmt.Errorf("YARN 주소(yarn_url)가 설정되지 않았습니다")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if s := strings.TrimSpace(states); s != "" {
		q.Set("states", s)
	}
	req, err := h.newRequest(ctx, http.MethodGet, opsapi.JoinURL(base, "/ws/v1/cluster/apps", q))
	if err != nil {
		return nil, err
	}
	var out struct {
		Apps struct {
			App []struct {
				ID          string  `json:"id"`
				Name        string  `json:"name"`
				User        string  `json:"user"`
				Queue       string  `json:"queue"`
				State       string  `json:"state"`
				Progress    float64 `json:"progress"`
				StartedTime int64   `json:"startedTime"`
			} `json:"app"`
		} `json:"apps"`
	}
	if err := opsapi.DoJSON(ctx, h.client, req, &out); err != nil {
		return nil, err
	}
	apps := make([]App, 0, len(out.Apps.App))
	for _, a := range out.Apps.App {
		app := App{
			ID: a.ID, Name: a.Name, User: a.User, Queue: a.Queue,
			State: a.State, Progress: a.Progress,
		}
		if a.StartedTime > 0 {
			app.StartAt = time.UnixMilli(a.StartedTime).UTC()
		}
		apps = append(apps, app)
	}
	return apps, nil
}

// List는 HDFS 경로의 목록이다.
func (h *Hadoop) List(ctx context.Context, p string) ([]Entry, error) {
	p = CleanPath(p)
	u := opsapi.JoinURL(h.cfg.BaseURL(), webhdfsPath(p), h.query(url.Values{"op": {"LISTSTATUS"}}))
	req, err := h.newRequest(ctx, http.MethodGet, u)
	if err != nil {
		return nil, err
	}
	var out struct {
		FileStatuses struct {
			FileStatus []struct {
				PathSuffix       string `json:"pathSuffix"`
				Type             string `json:"type"`
				Length           int64  `json:"length"`
				Owner            string `json:"owner"`
				Group            string `json:"group"`
				Permission       string `json:"permission"`
				Replication      int    `json:"replication"`
				ModificationTime int64  `json:"modificationTime"`
			} `json:"FileStatus"`
		} `json:"FileStatuses"`
	}
	if err := opsapi.DoJSON(ctx, h.client, req, &out); err != nil {
		return nil, hdfsError(err)
	}
	entries := make([]Entry, 0, len(out.FileStatuses.FileStatus))
	for _, f := range out.FileStatuses.FileStatus {
		e := Entry{
			Name: f.PathSuffix, Dir: f.Type == "DIRECTORY", Size: f.Length,
			Owner: f.Owner, Group: f.Group, Permission: f.Permission,
			Replication: f.Replication,
		}
		if f.ModificationTime > 0 {
			e.ModifiedAt = time.UnixMilli(f.ModificationTime).UTC()
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Summary는 경로 아래의 합계와 쿼터다.
func (h *Hadoop) Summary(ctx context.Context, p string) (*PathSummary, error) {
	p = CleanPath(p)
	u := opsapi.JoinURL(h.cfg.BaseURL(), webhdfsPath(p), h.query(url.Values{"op": {"GETCONTENTSUMMARY"}}))
	req, err := h.newRequest(ctx, http.MethodGet, u)
	if err != nil {
		return nil, err
	}
	var out struct {
		ContentSummary struct {
			DirectoryCount int64 `json:"directoryCount"`
			FileCount      int64 `json:"fileCount"`
			Length         int64 `json:"length"`
			Quota          int64 `json:"quota"`
			SpaceConsumed  int64 `json:"spaceConsumed"`
			SpaceQuota     int64 `json:"spaceQuota"`
		} `json:"ContentSummary"`
	}
	if err := opsapi.DoJSON(ctx, h.client, req, &out); err != nil {
		return nil, hdfsError(err)
	}
	c := out.ContentSummary
	return &PathSummary{
		Path: p, Files: c.FileCount, Directories: c.DirectoryCount,
		Length: c.Length, SpaceUsed: c.SpaceConsumed,
		Quota: c.Quota, SpaceQuota: c.SpaceQuota,
	}, nil
}

// Mkdir는 디렉터리를 만든다.
func (h *Hadoop) Mkdir(ctx context.Context, p string) error {
	p = CleanPath(p)
	if p == "/" {
		return fmt.Errorf("만들 경로를 입력하세요")
	}
	u := opsapi.JoinURL(h.cfg.BaseURL(), webhdfsPath(p), h.query(url.Values{"op": {"MKDIRS"}}))
	req, err := h.newRequest(ctx, http.MethodPut, u)
	if err != nil {
		return err
	}
	var out struct {
		Boolean bool `json:"boolean"`
	}
	if err := opsapi.DoJSON(ctx, h.client, req, &out); err != nil {
		return hdfsError(err)
	}
	if !out.Boolean {
		return fmt.Errorf("만들지 못했습니다 (이미 있거나 권한이 없습니다)")
	}
	return nil
}

// Rename은 경로 이름을 바꾼다(같은 파일시스템 안에서의 이동이기도 하다).
func (h *Hadoop) Rename(ctx context.Context, from, to string) error {
	from, to = CleanPath(from), CleanPath(to)
	if from == "/" || to == "/" {
		return fmt.Errorf("루트는 이름을 바꿀 수 없습니다")
	}
	u := opsapi.JoinURL(h.cfg.BaseURL(), webhdfsPath(from),
		h.query(url.Values{"op": {"RENAME"}, "destination": {to}}))
	req, err := h.newRequest(ctx, http.MethodPut, u)
	if err != nil {
		return err
	}
	var out struct {
		Boolean bool `json:"boolean"`
	}
	if err := opsapi.DoJSON(ctx, h.client, req, &out); err != nil {
		return hdfsError(err)
	}
	if !out.Boolean {
		return fmt.Errorf("이름을 바꾸지 못했습니다 (대상이 이미 있거나 권한이 없습니다)")
	}
	return nil
}

// Delete는 경로를 지운다. recursive가 아니면 빈 디렉터리와 파일만 지워진다.
func (h *Hadoop) Delete(ctx context.Context, p string, recursive bool) error {
	p = CleanPath(p)
	if p == "/" {
		// 루트 삭제는 클러스터의 모든 데이터를 지우는 명령이다. 이 앱은 그 요청을
		// 전달하지 않는다 — 실수로 누를 수 있는 자리에 둘 만한 조작이 아니다.
		return fmt.Errorf("루트(/)는 지울 수 없습니다")
	}
	u := opsapi.JoinURL(h.cfg.BaseURL(), webhdfsPath(p),
		h.query(url.Values{"op": {"DELETE"}, "recursive": {strconv.FormatBool(recursive)}}))
	req, err := h.newRequest(ctx, http.MethodDelete, u)
	if err != nil {
		return err
	}
	var out struct {
		Boolean bool `json:"boolean"`
	}
	if err := opsapi.DoJSON(ctx, h.client, req, &out); err != nil {
		return hdfsError(err)
	}
	if !out.Boolean {
		return fmt.Errorf("지우지 못했습니다 (비어 있지 않은 디렉터리이거나 권한이 없습니다)")
	}
	return nil
}

// Metrics는 폴러가 쓰는 지표다.
type HadoopMetrics struct {
	Capacity       Capacity
	LiveNodes      float64
	DeadNodes      float64
	MissingBlocks  float64
	UnderRepBlocks float64
	CorruptBlocks  float64
	Safemode       bool
	Health         Health
	YARN           *yarnMetrics
}

// Collect는 지표 수집용으로 필요한 값만 모은다.
func (h *Hadoop) Collect(ctx context.Context) (*HadoopMetrics, error) {
	ov, err := h.Overview(ctx)
	if err != nil {
		return nil, err
	}
	info, _ := h.jmxBean(ctx, "Hadoop:service=NameNode,name=NameNodeInfo")
	state, _ := h.jmxBean(ctx, "Hadoop:service=NameNode,name=FSNamesystemState")
	fs, _ := h.jmxBean(ctx, "Hadoop:service=NameNode,name=FSNamesystem")

	m := &HadoopMetrics{
		Capacity:       ov.Capacity,
		Health:         ov.Health,
		LiveNodes:      float64(num64(state["NumLiveDataNodes"])),
		DeadNodes:      float64(num64(state["NumDeadDataNodes"])),
		MissingBlocks:  float64(num64(fs["MissingBlocks"])),
		UnderRepBlocks: float64(num64(fs["UnderReplicatedBlocks"])),
		CorruptBlocks:  float64(num64(fs["CorruptBlocks"])),
		Safemode:       strings.TrimSpace(str(info["Safemode"])) != "",
	}
	if h.yarnURL() != "" {
		if y, err := h.yarnMetrics(ctx); err == nil {
			m.YARN = y
		}
	}
	return m, nil
}

// webhdfsPath는 HDFS 경로를 WebHDFS URL 경로로 만든다.
func webhdfsPath(p string) string {
	clean := CleanPath(p)
	// 각 구성요소를 따로 인코딩한다. 통째로 인코딩하면 구분자(/)까지 %2F가 되어
	// 서버가 한 덩어리 이름으로 읽는다.
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return "/webhdfs/v1/" + strings.Join(parts, "/")
}

// CleanPath는 HDFS 경로를 정규화한다.
//
// 상위 이동(..)을 정리하는 이유는 보안이 아니라 정확성이다. WebHDFS는 ".."를 이름으로
// 볼 수도 있고 서버 구현에 따라 다르게 다룬다 — 화면에서 위로 올라가는 조작이 종류마다
// 다르게 동작하면 사용자는 자기가 어디 있는지 알 수 없게 된다.
func CleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	clean := path.Clean(p)
	if clean == "." {
		return "/"
	}
	return clean
}

// hdfsError는 WebHDFS의 RemoteException을 사람이 읽을 말로 바꾼다.
//
// 그대로 두면 "org.apache.hadoop.security.AccessControlException: Permission denied:
// user=dr.who…" 같은 줄이 화면에 뜬다. 원문도 남기되 앞에 무엇을 해야 하는지 적는다.
func hdfsError(err error) error {
	var he *opsapi.HTTPError
	if !asHTTPError(err, &he) {
		return err
	}
	var payload struct {
		RemoteException struct {
			Exception string `json:"exception"`
			Message   string `json:"message"`
		} `json:"RemoteException"`
	}
	_ = json.Unmarshal([]byte(he.Body), &payload)
	ex := payload.RemoteException.Exception
	msg := strings.TrimSpace(payload.RemoteException.Message)
	if msg == "" {
		msg = opsapi.Snippet(he.Body)
	}
	switch {
	case strings.Contains(ex, "AccessControlException"):
		return fmt.Errorf("HDFS 권한이 없습니다. 커넥션의 계정이 이 경로의 소유자·그룹에 맞는지 확인하세요: %s", msg)
	case strings.Contains(ex, "FileNotFoundException"):
		return fmt.Errorf("경로를 찾을 수 없습니다: %s", msg)
	case strings.Contains(ex, "SafeModeException"):
		return fmt.Errorf("NameNode가 세이프모드라 변경할 수 없습니다: %s", msg)
	case he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden:
		return fmt.Errorf("인증이 거부됐습니다(%d). 계정을 비워 두면 서버 기본 사용자로 접속합니다: %s",
			he.Status, msg)
	}
	return fmt.Errorf("HDFS 오류(%d): %s", he.Status, msg)
}

// str/num64는 JMX 값의 타입이 버전마다 다른 것을 흡수한다(숫자가 문자열로 오기도 한다).
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func num64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err == nil {
			return n
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err == nil {
			return int64(f)
		}
	}
	return 0
}
