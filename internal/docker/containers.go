package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 컨테이너·볼륨·이미지 조작.
//
// 요청 본문은 도커가 정한 모양을 그대로 쓴다. 우리 말로 바꿔 담지 않는 이유:
// 이 구조체들은 우리가 설계한 것이 아니라 **남의 API 의 모양**이고, 중간에 우리
// 이름을 하나 끼우면 도커 문서를 읽으면서 우리 이름으로 번역하는 일이 늘어난다.
// 필드 이름이 Go 답지 않은 것(HostConfig, ExposedPorts)은 그래서다.

// ---------- 이미지 ----------

// ImageExists는 그 이미지가 이미 내려와 있는지 본다.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.getJSON(ctx, "/"+APIVersion+"/images/"+url.PathEscape(ref)+"/json",
		nil, "이미지 확인")
	if err == nil {
		return true, nil
	}
	if IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// PullProgress는 내려받는 중 한 줄이다.
type PullProgress struct {
	// Status는 도커가 준 상태 문구다("Downloading", "Pull complete" …).
	Status string `json:"status"`
	ID     string `json:"id"`
	// Detail은 진행률이다(있을 때만).
	Detail struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
	Error string `json:"error"`
}

// PullImage는 이미지를 내려받는다.
//
// 진행 상황을 콜백으로 흘린다. 다 받고 나서 한 번에 알려 주면, 큰 이미지를
// 받는 동안(ClickHouse 는 압축해서 300MB 가 넘는다) 화면에는 아무 일도
// 일어나지 않는 것으로 보인다.
//
// 도커는 **HTTP 200 을 먼저 주고 본문에 오류를 적는다.** 스트리밍 API 라
// 머리말을 보낼 때는 아직 실패를 모르기 때문이다. 그래서 본문의 error 를 반드시
// 읽어야 한다 — 상태 코드만 보면 없는 이미지를 받아도 성공으로 보인다.
func (c *Client) PullImage(ctx context.Context, ref string, onProgress func(PullProgress)) error {
	path := "/" + APIVersion + "/images/create" + q(map[string]string{"fromImage": ref})
	res, err := c.do(ctx, http.MethodPost, path, nil, "이미지 내려받기")
	if err != nil {
		return err
	}
	defer res.Body.Close()

	dec := json.NewDecoder(res.Body)
	for {
		var p PullProgress
		if err := dec.Decode(&p); err != nil {
			if err == io.EOF {
				return nil
			}
			if ctx.Err() != nil {
				return fmt.Errorf("이미지 내려받기: %w", ctx.Err())
			}
			return fmt.Errorf("이미지 내려받기: 진행 상황을 읽지 못했습니다: %w", err)
		}
		if p.Error != "" {
			return &Error{Status: http.StatusBadRequest, Message: p.Error, Op: "이미지 내려받기"}
		}
		if onProgress != nil {
			onProgress(p)
		}
	}
}

// ---------- 컨테이너 ----------

// PortBinding은 호스트 쪽 포트다.
type PortBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort"`
}

// RestartPolicy는 컨테이너가 죽었을 때의 처리다.
type RestartPolicy struct {
	Name              string `json:"Name"` // no | always | unless-stopped | on-failure
	MaximumRetryCount int    `json:"MaximumRetryCount,omitempty"`
}

// Mount는 볼륨 또는 바인드 마운트다.
type Mount struct {
	Type     string `json:"Type"` // volume | bind | tmpfs
	Source   string `json:"Source,omitempty"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

// HostConfig는 컨테이너 밖의 설정이다(자원·포트·마운트).
type HostConfig struct {
	PortBindings  map[string][]PortBinding `json:"PortBindings,omitempty"`
	Mounts        []Mount                  `json:"Mounts,omitempty"`
	RestartPolicy *RestartPolicy           `json:"RestartPolicy,omitempty"`
	// Memory는 바이트 단위 상한이다. 0 은 제한 없음.
	Memory int64 `json:"Memory,omitempty"`
	// NanoCPUs는 10억분의 1 코어 단위다(1코어 = 1e9).
	NanoCPUs int64 `json:"NanoCpus,omitempty"`
	// Ulimits는 파일 핸들 상한 같은 것이다. ClickHouse 가 이것을 요구한다.
	Ulimits []Ulimit `json:"Ulimits,omitempty"`
	// GroupAdd는 컨테이너 안 프로세스에 더해 줄 그룹이다.
	GroupAdd []string `json:"GroupAdd,omitempty"`
}

// Ulimit은 자원 상한 하나다.
type Ulimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

// HealthConfig는 헬스체크다.
type HealthConfig struct {
	Test []string `json:"Test,omitempty"` // ["CMD-SHELL", "..."]
	// 나노초 단위다. 도커 API 가 그렇게 받는다.
	Interval    int64 `json:"Interval,omitempty"`
	Timeout     int64 `json:"Timeout,omitempty"`
	StartPeriod int64 `json:"StartPeriod,omitempty"`
	Retries     int   `json:"Retries,omitempty"`
}

// CreateRequest는 컨테이너를 만드는 요청이다.
type CreateRequest struct {
	Image        string              `json:"Image"`
	Env          []string            `json:"Env,omitempty"` // "KEY=value"
	Cmd          []string            `json:"Cmd,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Healthcheck  *HealthConfig       `json:"Healthcheck,omitempty"`
	HostConfig   *HostConfig         `json:"HostConfig,omitempty"`
	// NetworkingConfig는 붙일 네트워크다.
	NetworkingConfig *NetworkingConfig `json:"NetworkingConfig,omitempty"`
}

// NetworkingConfig는 만들 때 붙일 네트워크들이다.
type NetworkingConfig struct {
	EndpointsConfig map[string]struct{} `json:"EndpointsConfig,omitempty"`
}

// CreateResponse는 만든 결과다.
type CreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

// CreateContainer는 컨테이너를 만든다(시작하지는 않는다).
//
// 만들기와 시작하기를 갈라 둔 이유: 그 사이에 설정 파일을 넣어야 하는 DB 가
// 있다(ClickHouse 의 config.d). 시작한 뒤에 넣으면 이미 읽고 지나간 뒤다.
func (c *Client) CreateContainer(ctx context.Context, name string, req *CreateRequest) (*CreateResponse, error) {
	path := "/" + APIVersion + "/containers/create" + q(map[string]string{"name": name})
	var out CreateResponse
	if err := c.postJSON(ctx, path, req, &out, "컨테이너 만들기"); err != nil {
		return nil, err
	}
	return &out, nil
}

// StartContainer는 컨테이너를 시작한다.
//
// 이미 도는 중이면(304) 오류로 보지 않는다. 부르는 쪽이 원한 상태가 이미
// 그것이므로, 실패라고 말하면 "시작" 단추를 두 번 누른 사람에게 오류가 뜬다.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	res, err := c.do(ctx, http.MethodPost,
		"/"+APIVersion+"/containers/"+url.PathEscape(id)+"/start", nil, "컨테이너 시작")
	if err != nil {
		if IsConflict(err) {
			return nil
		}
		return err
	}
	return res.Body.Close()
}

// StopContainer는 컨테이너를 멈춘다. timeout 초 뒤에 강제로 끊는다.
//
// 이미 멈춰 있으면(304) 오류로 보지 않는다 — 시작과 같은 이유다.
func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if secs <= 0 {
		secs = 10
	}
	path := "/" + APIVersion + "/containers/" + url.PathEscape(id) + "/stop" +
		q(map[string]string{"t": strconv.Itoa(secs)})
	res, err := c.do(ctx, http.MethodPost, path, nil, "컨테이너 멈추기")
	if err != nil {
		if IsConflict(err) || IsNotModified(err) {
			return nil
		}
		return err
	}
	return res.Body.Close()
}

// RestartContainer는 멈추고 다시 시작한다.
func (c *Client) RestartContainer(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if secs <= 0 {
		secs = 10
	}
	path := "/" + APIVersion + "/containers/" + url.PathEscape(id) + "/restart" +
		q(map[string]string{"t": strconv.Itoa(secs)})
	res, err := c.do(ctx, http.MethodPost, path, nil, "컨테이너 다시 시작")
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// RemoveContainer는 컨테이너를 지운다.
//
// volumes 는 **이름 없는** 볼륨만 지운다. 우리가 이름을 붙여 만든 데이터
// 볼륨은 이것으로 사라지지 않는다 — 데이터를 지우는 것은 따로 물어야 하는
// 일이고, 컨테이너를 지우는 것에 딸려 오면 안 된다.
func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	path := "/" + APIVersion + "/containers/" + url.PathEscape(id) +
		q(map[string]string{"force": boolParam(force), "v": "true"})
	res, err := c.do(ctx, http.MethodDelete, path, nil, "컨테이너 지우기")
	if err != nil {
		if IsNotFound(err) {
			return nil // 없으면 지운 것과 같다
		}
		return err
	}
	return res.Body.Close()
}

// ContainerState는 컨테이너의 지금 상태다.
type ContainerState struct {
	Status     string `json:"Status"` // created|running|paused|restarting|removing|exited|dead
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	Restarting bool   `json:"Restarting"`
	ExitCode   int    `json:"ExitCode"`
	Error      string `json:"Error"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
	Health     *struct {
		Status        string `json:"Status"` // starting|healthy|unhealthy
		FailingStreak int    `json:"FailingStreak"`
		Log           []struct {
			Start    string `json:"Start"`
			ExitCode int    `json:"ExitCode"`
			Output   string `json:"Output"`
		} `json:"Log"`
	} `json:"Health"`
}

// Inspect는 컨테이너 하나를 살펴본 결과다. 우리가 쓰는 것만 담는다.
type Inspect struct {
	ID      string          `json:"Id"`
	Name    string          `json:"Name"`
	Created string          `json:"Created"`
	State   *ContainerState `json:"State"`
	Config  struct {
		Image  string            `json:"Image"`
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
		Tty    bool              `json:"Tty"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings  map[string][]PortBinding `json:"PortBindings"`
		RestartPolicy *RestartPolicy           `json:"RestartPolicy"`
		Memory        int64                    `json:"Memory"`
		NanoCPUs      int64                    `json:"NanoCpus"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	// NetworkSettings는 **실제로 붙은** 것들이다.
	//
	// HostConfig.PortBindings 와 갈라 두어야 한다. 저쪽은 우리가 요청한 값이고
	// 이쪽이 결과다 — 호스트 포트를 0 으로 요청해 도커가 골라 주게 하면,
	// 요청 쪽에는 그대로 "0" 이 남고 고른 포트는 여기에만 있다.
	NetworkSettings struct {
		Ports    map[string][]PortBinding `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// HostPort는 그 컨테이너 포트가 호스트의 몇 번에 붙었는지다.
//
// **결과를 본다.** 요청(HostConfig.PortBindings)을 보면 도커가 골라 준 포트를
// 0 으로 읽는다 — 그 값으로 커넥션을 등록하면 붙을 수 없는 커넥션이 만들어지고,
// 실패는 등록할 때가 아니라 나중에 접속을 눌렀을 때 난다.
//
// port 는 "6379/tcp" 꼴이다. 프로토콜을 빼고 부르면 tcp 로 본다.
func (i *Inspect) HostPort(port string) string {
	if i == nil {
		return ""
	}
	if !strings.Contains(port, "/") {
		port += "/tcp"
	}
	for _, b := range i.NetworkSettings.Ports[port] {
		// IPv6 쪽도 함께 오는 일이 있다. 값이 있는 첫 번째를 쓴다.
		if b.HostPort != "" && b.HostPort != "0" {
			return b.HostPort
		}
	}
	return ""
}

// IPAddress는 컨테이너의 네트워크 주소다(네트워크를 하나만 쓸 때).
//
// 왜 필요한가: 같은 도커 안의 다른 컨테이너가 이 DB 에 붙을 때는 호스트
// 포트가 아니라 컨테이너 이름과 안쪽 포트로 붙는다. DB Studio 자신이
// 컨테이너로 돌고 있으면 그쪽이 오히려 정상 경로다.
func (i *Inspect) IPAddress() string {
	if i == nil {
		return ""
	}
	for _, n := range i.NetworkSettings.Networks {
		if n.IPAddress != "" {
			return n.IPAddress
		}
	}
	return ""
}

// HealthStatus는 헬스체크 결과다(없으면 빈 문자열).
func (i *Inspect) HealthStatus() string {
	if i == nil || i.State == nil || i.State.Health == nil {
		return ""
	}
	return i.State.Health.Status
}

// ShortName은 앞의 슬래시를 뗀 이름이다. 도커는 "/이름" 으로 돌려준다.
func (i *Inspect) ShortName() string {
	if i == nil {
		return ""
	}
	return strings.TrimPrefix(i.Name, "/")
}

// InspectContainer는 컨테이너 하나를 살펴본다.
func (c *Client) InspectContainer(ctx context.Context, id string) (*Inspect, error) {
	var out Inspect
	if err := c.getJSON(ctx, "/"+APIVersion+"/containers/"+url.PathEscape(id)+"/json",
		&out, "컨테이너 살펴보기"); err != nil {
		return nil, err
	}
	return &out, nil
}

// ContainerSummary는 목록의 한 줄이다.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

// ListContainers는 라벨로 걸러 컨테이너를 찾는다.
//
// 라벨로 찾는 이유: 이름으로 찾으면 사람이 도커에서 직접 이름을 바꾼 순간
// 우리 목록에서 사라진다. 라벨은 이름을 바꿔도 남는다.
func (c *Client) ListContainers(ctx context.Context, labels map[string]string) ([]ContainerSummary, error) {
	filters := map[string][]string{}
	for k, v := range labels {
		key := k
		if v != "" {
			key = k + "=" + v
		}
		filters["label"] = append(filters["label"], key)
	}
	raw, err := json.Marshal(filters)
	if err != nil {
		return nil, fmt.Errorf("컨테이너 목록: 조건을 만들지 못했습니다: %w", err)
	}
	path := "/" + APIVersion + "/containers/json" + q(map[string]string{
		"all": "true", "filters": string(raw),
	})
	var out []ContainerSummary
	if err := c.getJSON(ctx, path, &out, "컨테이너 목록"); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------- 볼륨 ----------

// CreateVolume은 이름 붙인 볼륨을 만든다(이미 있으면 그것을 돌려준다).
//
// 도커는 같은 이름으로 다시 만들어도 오류를 내지 않는다. 그래서 "있으면
// 만들지 않는다"를 우리가 따로 확인할 필요가 없다.
func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{"Name": name, "Labels": labels}
	return c.postJSON(ctx, "/"+APIVersion+"/volumes/create", body, nil, "볼륨 만들기")
}

// RemoveVolume은 볼륨을 지운다. 데이터가 사라진다.
func (c *Client) RemoveVolume(ctx context.Context, name string, force bool) error {
	path := "/" + APIVersion + "/volumes/" + url.PathEscape(name) +
		q(map[string]string{"force": boolParam(force)})
	res, err := c.do(ctx, http.MethodDelete, path, nil, "볼륨 지우기")
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	return res.Body.Close()
}

// ---------- 네트워크 ----------

// EnsureNetwork는 그 이름의 네트워크가 있게 한다(있으면 그대로 둔다).
//
// 왜 필요한가: 만든 DB 에 다른 컨테이너가 이름으로 붙을 수 있어야 한다.
// 기본 브리지에서는 이름으로 찾을 수 없고 IP 를 알아야 한다.
func (c *Client) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{
		"Name":           name,
		"Driver":         "bridge",
		"CheckDuplicate": true,
		"Labels":         labels,
	}
	err := c.postJSON(ctx, "/"+APIVersion+"/networks/create", body, nil, "네트워크 만들기")
	if err == nil {
		return nil
	}
	// 이미 있으면 409 다. 우리가 원한 상태가 이미 그것이므로 실패가 아니다.
	if IsConflict(err) {
		return nil
	}
	return err
}

// ---------- 유틸 ----------

// IsNotModified는 오류가 304(이미 그 상태다)인지 본다.
func IsNotModified(err error) bool {
	var de *Error
	if !errors.As(err, &de) {
		return false
	}
	return de.Status == http.StatusNotModified
}

func boolParam(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
