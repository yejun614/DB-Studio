// Package hostmon은 **DB Studio가 도는 컴퓨터 자신**의 상태를 읽는다.
//
// 왜 여기까지 보는가: DB가 느려졌을 때 원인은 DB 안에만 있지 않다. 디스크가 차서
// 쓰기가 막히거나, 다른 프로세스가 CPU를 다 쓰거나, 네트워크가 포화된 것이 흔하다.
// 그런데 그 사실을 확인하려면 이 화면을 떠나 서버에 로그인해야 했다 — 장애 중에
// 가장 하고 싶지 않은 일이다.
//
// 외부 라이브러리를 쓰지 않는 이유는 이 저장소의 다른 선택과 같다. 필요한 것은
// "몇 퍼센트인가" 정도이고, 그 값은 OS마다 파일 하나 또는 함수 하나에서 나온다.
// 대신 **읽지 못한 것은 읽지 못했다고 말한다**(Notes) — 0으로 채우면 화면이
// "한가한 서버"라고 거짓말을 한다.
package hostmon

import (
	"runtime"
	"sort"
	"time"
)

// Disk는 마운트 지점 하나의 사용량이다.
type Disk struct {
	Mount string `json:"mount"`
	// FS는 파일시스템 종류다(있으면). 임시 파일시스템을 걸러내는 근거이자 표시용.
	FS    string `json:"fs,omitempty"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// UsedPercent는 0..100이다. 크기를 모르면 0을 돌려준다.
func (d Disk) UsedPercent() float64 {
	if d.Total == 0 {
		return 0
	}
	return float64(d.Total-d.Free) / float64(d.Total) * 100
}

// NIC는 네트워크 인터페이스 하나의 **누적** 바이트 수다.
// 초당 얼마인지는 두 표본의 차이로 계산한다(Collector.Sample).
type NIC struct {
	Name string `json:"name"`
	RX   uint64 `json:"rx"`
	TX   uint64 `json:"tx"`
}

// Info는 잘 바뀌지 않는 값이다. 화면 머리에 한 번 보여준다.
type Info struct {
	Hostname string    `json:"hostname"`
	OS       string    `json:"os"`
	Arch     string    `json:"arch"`
	CPUs     int       `json:"cpus"`
	BootAt   time.Time `json:"bootAt,omitempty"`
}

// Snapshot은 한 시점의 읽기 결과다.
type Snapshot struct {
	At time.Time `json:"at"`
	// CPUPercent는 **표본 사이 구간의 평균 사용률**이다(0..100).
	// 첫 표본에는 비교 대상이 없어 nil이다 — 0으로 채우면 "방금 켠 서버는 한가하다"는
	// 거짓이 차트 맨 앞에 박힌다.
	CPUPercent *float64 `json:"cpuPercent"`
	Load1      *float64 `json:"load1,omitempty"`

	MemTotal uint64 `json:"memTotal"`
	MemUsed  uint64 `json:"memUsed"`
	// SwapTotal이 0이면 스왑이 없거나 읽지 못한 것이다.
	SwapTotal uint64 `json:"swapTotal,omitempty"`
	SwapUsed  uint64 `json:"swapUsed,omitempty"`

	Disks []Disk `json:"disks"`
	NICs  []NIC  `json:"nics"`
	// NetRXRate/NetTXRate는 초당 바이트다(모든 인터페이스 합). 첫 표본에는 없다.
	NetRXRate *float64 `json:"netRxRate,omitempty"`
	NetTXRate *float64 `json:"netTxRate,omitempty"`

	// ProcRSS는 DB Studio 자신이 쓰는 메모리다. 호스트가 아니라 이 프로세스 몫이라
	// "서버가 무겁다"의 책임이 우리에게 있는지 가른다.
	ProcRSS uint64 `json:"procRss"`

	Info  Info     `json:"info"`
	Notes []string `json:"notes,omitempty"`
}

// MemUsedPercent는 0..100이다.
func (s *Snapshot) MemUsedPercent() float64 {
	if s.MemTotal == 0 {
		return 0
	}
	return float64(s.MemUsed) / float64(s.MemTotal) * 100
}

// raw는 플랫폼 구현이 채우는 값이다. 누적 카운터를 그대로 담는다.
type raw struct {
	cpuBusy  uint64 // 누적 사용 시간 (단위는 플랫폼마다 다르나 비율만 쓰므로 무관)
	cpuTotal uint64
	load1    *float64

	memTotal, memUsed   uint64
	swapTotal, swapUsed uint64

	disks  []Disk
	nics   []NIC
	bootAt time.Time
	notes  []string
}

// Collector는 표본 사이의 차이가 필요한 값(CPU·네트워크)을 위해 직전 표본을 들고 있다.
type Collector struct {
	prev   *raw
	prevAt time.Time
}

func New() *Collector { return &Collector{} }

// Sample은 지금 상태를 읽는다.
//
// 오류를 반환하지 않는 이유: 일부를 못 읽는 것이 정상이다(권한, 컨테이너, 플랫폼).
// 전부 실패해도 화면은 떠야 하고, 무엇을 못 읽었는지는 Notes로 전한다.
func (c *Collector) Sample() *Snapshot {
	now := time.Now()
	r := readHost()

	s := &Snapshot{
		At:        now,
		Load1:     r.load1,
		MemTotal:  r.memTotal,
		MemUsed:   r.memUsed,
		SwapTotal: r.swapTotal,
		SwapUsed:  r.swapUsed,
		Disks:     r.disks,
		NICs:      r.nics,
		ProcRSS:   procRSS(),
		Notes:     r.notes,
		Info: Info{
			Hostname: hostname(),
			OS:       runtime.GOOS,
			Arch:     runtime.GOARCH,
			CPUs:     runtime.NumCPU(),
			BootAt:   r.bootAt,
		},
	}

	if c.prev != nil {
		if p := cpuPercent(c.prev, &r); p != nil {
			s.CPUPercent = p
		}
		if secs := now.Sub(c.prevAt).Seconds(); secs > 0 {
			rx, tx := netRates(c.prev.nics, r.nics, secs)
			s.NetRXRate, s.NetTXRate = rx, tx
		}
	}
	c.prev, c.prevAt = &r, now
	return s
}

// cpuPercent는 두 표본 사이의 사용률이다.
func cpuPercent(prev, cur *raw) *float64 {
	if cur.cpuTotal <= prev.cpuTotal {
		// 카운터가 되감겼거나(재부팅) 읽지 못했다. 억지로 값을 만들지 않는다.
		return nil
	}
	total := float64(cur.cpuTotal - prev.cpuTotal)
	busy := float64(0)
	if cur.cpuBusy >= prev.cpuBusy {
		busy = float64(cur.cpuBusy - prev.cpuBusy)
	}
	p := busy / total * 100
	return clampPercent(p)
}

// netRates는 인터페이스별 누적값의 차이를 합쳐 초당 바이트로 만든다.
//
// 이름으로 짝을 맞춘다. 표본 사이에 인터페이스가 생기거나 사라지면 그것만 빠진다 —
// 전체를 버리면 랜선 하나 뽑았다고 그래프가 끊긴다.
func netRates(prev, cur []NIC, secs float64) (*float64, *float64) {
	if len(prev) == 0 || len(cur) == 0 {
		return nil, nil
	}
	before := make(map[string]NIC, len(prev))
	for _, n := range prev {
		before[n.Name] = n
	}
	var rx, tx float64
	matched := false
	for _, n := range cur {
		p, ok := before[n.Name]
		if !ok {
			continue
		}
		matched = true
		// 카운터가 줄었으면 되감긴 것이다(32비트 카운터는 4GB에서 넘어간다).
		// 그 구간만 버린다 — 음수 속도를 그리는 것보다 낫다.
		if n.RX >= p.RX {
			rx += float64(n.RX - p.RX)
		}
		if n.TX >= p.TX {
			tx += float64(n.TX - p.TX)
		}
	}
	if !matched {
		return nil, nil
	}
	rxRate := rx / secs
	txRate := tx / secs
	return &rxRate, &txRate
}

func clampPercent(p float64) *float64 {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return &p
}

// sortDisks는 화면 순서를 고정한다. 순서가 폴링마다 바뀌면 눈이 따라가지 못한다.
func sortDisks(d []Disk) []Disk {
	sort.Slice(d, func(i, j int) bool { return d[i].Mount < d[j].Mount })
	return d
}
