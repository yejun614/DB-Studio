package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version은 데몬의 판본이다.
type Version struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	GitCommit     string `json:"GitCommit"`
	GoVersion     string `json:"GoVersion"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
	KernelVersion string `json:"KernelVersion"`
}

// Info는 데몬의 상태다. 우리가 쓰는 것만 담는다.
//
// 응답 전체는 12KB 를 넘고 대부분이 우리가 쓰지 않는 것들이다. 다 담아 두면
// 도커가 필드를 하나 고칠 때마다 이 구조체를 따라 고쳐야 한다.
type Info struct {
	ID                string   `json:"ID"`
	Name              string   `json:"Name"`
	ServerVersion     string   `json:"ServerVersion"`
	Containers        int      `json:"Containers"`
	ContainersRunning int      `json:"ContainersRunning"`
	ContainersPaused  int      `json:"ContainersPaused"`
	ContainersStopped int      `json:"ContainersStopped"`
	Images            int      `json:"Images"`
	NCPU              int      `json:"NCPU"`
	MemTotal          int64    `json:"MemTotal"`
	OperatingSystem   string   `json:"OperatingSystem"`
	Architecture      string   `json:"Architecture"`
	DockerRootDir     string   `json:"DockerRootDir"`
	Driver            string   `json:"Driver"`
	CgroupVersion     string   `json:"CgroupVersion"`
	Warnings          []string `json:"Warnings"`
	Swarm             struct {
		NodeID           string `json:"NodeID"`
		LocalNodeState   string `json:"LocalNodeState"` // inactive | pending | active | error | locked
		ControlAvailable bool   `json:"ControlAvailable"`
		Nodes            int    `json:"Nodes"`
		Managers         int    `json:"Managers"`
	} `json:"Swarm"`
}

// SwarmActive는 이 데몬이 스웜 모드인지다.
func (i *Info) SwarmActive() bool {
	return strings.EqualFold(i.Swarm.LocalNodeState, "active")
}

// Version은 데몬 판본을 읽는다.
//
// 버전을 붙이지 않고 부른다. 버전 협상을 하기 전이므로 우리가 쓰는 v1.43 을
// 붙이면 그보다 옛 데몬에서 이 호출부터 실패하고, 그러면 "얼마나 옛것인가"를
// 물어볼 길이 없다. /version 은 버전 없이도 언제나 답한다.
func (c *Client) Version(ctx context.Context) (*Version, error) {
	var out Version
	if err := c.getJSON(ctx, "/version", &out, "도커 판본 확인"); err != nil {
		return nil, err
	}
	return &out, nil
}

// Info는 데몬 상태를 읽는다.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	var out Info
	if err := c.getJSON(ctx, "/"+APIVersion+"/info", &out, "도커 상태 확인"); err != nil {
		return nil, err
	}
	return &out, nil
}

// Ping은 데몬이 살아 있는지만 본다.
func (c *Client) Ping(ctx context.Context) error {
	res, err := c.do(ctx, "GET", "/_ping", nil, "도커 확인")
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// Status는 "지금 도커를 쓸 수 있는가"에 대한 답이다.
//
// 화면이 물어보는 것이 이 하나다. 못 쓸 때 **무엇을 고쳐야 하는지**까지 담는다 —
// 못 쓴다는 사실만 알려 주면 사람은 다음에 무엇을 할지 알 수 없고, 이 기능은
// 배치를 손봐야 켜지는 종류라 그 안내가 기능의 절반이다.
type Status struct {
	// Reachable은 데몬에 닿았는지다.
	Reachable bool `json:"reachable"`
	// Socket은 우리가 본 소켓 경로다.
	Socket string `json:"socket"`
	// Reason은 못 닿은 이유(사람 말)다. 닿았으면 빈 문자열.
	Reason string `json:"reason,omitempty"`

	Version       string `json:"version,omitempty"`
	APIVersion    string `json:"apiVersion,omitempty"`
	MinAPIVersion string `json:"minApiVersion,omitempty"`
	// APIOK는 우리가 쓰는 API 버전을 이 데몬이 받는지다.
	APIOK bool `json:"apiOk"`

	OS           string `json:"os,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	Containers   int    `json:"containers"`
	Running      int    `json:"running"`
	Images       int    `json:"images"`
	CPUs         int    `json:"cpus"`
	MemTotal     int64  `json:"memTotal"`

	// SwarmState는 inactive | active | pending | error | locked 다.
	SwarmState string   `json:"swarmState,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// Status는 한 번의 왕복으로 쓸 수 있는지와 왜인지를 모아 온다.
//
// 오류를 돌려주지 않는다. "못 쓴다"는 것도 이 화면이 보여줄 답이라, 오류로
// 올리면 화면이 오류 처리와 상태 표시를 따로 만들게 된다.
func (c *Client) Status(ctx context.Context) *Status {
	st := &Status{Socket: c.socket}

	ver, err := c.Version(ctx)
	if err != nil {
		st.Reason = reasonOf(err)
		return st
	}
	st.Reachable = true
	st.Version = ver.Version
	st.APIVersion = ver.APIVersion
	st.MinAPIVersion = ver.MinAPIVersion
	st.OS = ver.Os
	st.Architecture = ver.Arch
	st.APIOK = apiAtLeast(ver.MinAPIVersion, APIVersion)
	if !st.APIOK {
		st.Reason = fmt.Sprintf("이 데몬은 API %s 이상만 받는데 이 앱은 %s 로 말합니다. "+
			"도커를 올리거나 이 앱을 올려야 합니다", ver.MinAPIVersion, APIVersion)
		return st
	}

	info, err := c.Info(ctx)
	if err != nil {
		// 판본은 읽혔는데 상태를 못 읽는 경우다. 닿기는 한 것이므로 Reachable 은
		// 그대로 두고 이유만 적는다 — 소켓 프록시가 /info 만 막아 둔 배치가 있다.
		st.Reason = "상태를 읽지 못했습니다: " + reasonOf(err)
		return st
	}
	st.Containers = info.Containers
	st.Running = info.ContainersRunning
	st.Images = info.Images
	st.CPUs = info.NCPU
	st.MemTotal = info.MemTotal
	st.SwarmState = info.Swarm.LocalNodeState
	st.Warnings = info.Warnings
	if st.OS == "" {
		st.OS = info.OperatingSystem
	}
	return st
}

// reasonOf는 오류에서 사람에게 보일 문장만 꺼낸다.
//
// 닿지 못한 오류는 이미 사람 말로 바꿔 둔 이유를 들고 있다(unreachable).
// 그것을 다시 감싸면 "도커에 닿지 못했습니다 (…): 도커에 닿지 못했습니다" 가 된다.
func reasonOf(err error) string {
	var ue *UnreachableError
	if errors.As(err, &ue) {
		return ue.Reason
	}
	return err.Error()
}

// apiAtLeast는 want 가 min 이상인지 본다("1.43" 꼴).
//
// 왜 직접 비교하는가: 이 버전은 세마버가 아니다. "1.43" 은 문자열로 견주면
// "1.5" 보다 작고(4 < 5), 그러면 훨씬 새 데몬을 옛것으로 읽는다.
func apiAtLeast(min, want string) bool {
	if strings.TrimSpace(min) == "" {
		return true // 데몬이 말해 주지 않으면 막지 않는다
	}
	wMaj, wMin := splitAPIVersion(want)
	mMaj, mMin := splitAPIVersion(min)
	if wMaj != mMaj {
		return wMaj > mMaj
	}
	return wMin >= mMin
}

func splitAPIVersion(v string) (int, int) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	major, minor := 0, 0
	if dot := strings.IndexByte(v, '.'); dot >= 0 {
		major, _ = strconv.Atoi(v[:dot])
		minor, _ = strconv.Atoi(v[dot+1:])
		return major, minor
	}
	major, _ = strconv.Atoi(v)
	return major, 0
}
