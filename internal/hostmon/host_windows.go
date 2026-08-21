//go:build windows

package hostmon

import (
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 윈도우에는 /proc이 없으므로 커널 함수를 직접 부른다.
//
// 고른 함수들은 모두 **오래되고 안 바뀌는** 것들이다(GetSystemTimes,
// GlobalMemoryStatusEx, GetDiskFreeSpaceEx, GetIfTable). 최신 API(MIB_IF_ROW2 등)는
// 구조체에 열거형과 비트필드가 섞여 있어 Go 구조체로 옮기다 한 칸만 어긋나도
// 엉뚱한 숫자가 그럴듯하게 나온다 — 그런 오류는 화면에서 알아볼 수 없다.

var (
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes = kernel32.NewProc("GetSystemTimes")
	procGlobalMemory   = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64 = kernel32.NewProc("GetTickCount64")
	procGetLogicalDrvs = kernel32.NewProc("GetLogicalDriveStringsW")
	procGetDriveType   = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFree    = kernel32.NewProc("GetDiskFreeSpaceExW")

	iphlpapi        = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetIfTable  = iphlpapi.NewProc("GetIfTable")
	procGetIfEntry  = iphlpapi.NewProc("GetIfEntry")
	_               = procGetIfEntry // 단건 조회는 쓰지 않는다(전체 표를 한 번에 읽는다)
	driveTypeFixed  = uint32(3)
	driveTypeRemote = uint32(4)
)

func readHost() raw {
	r := raw{}
	if err := winCPU(&r); err != nil {
		r.notes = append(r.notes, "CPU 사용률을 읽지 못했습니다: "+err.Error())
	}
	if err := winMem(&r); err != nil {
		r.notes = append(r.notes, "메모리 정보를 읽지 못했습니다: "+err.Error())
	}
	if err := winDisks(&r); err != nil {
		r.notes = append(r.notes, "디스크 정보를 읽지 못했습니다: "+err.Error())
	}
	if err := winNet(&r); err != nil {
		r.notes = append(r.notes, "네트워크 지표를 읽지 못했습니다: "+err.Error())
	}
	winBoot(&r)
	return r
}

// filetime을 100ns 단위 정수로 본다.
type filetime struct{ Low, High uint32 }

func (f filetime) u64() uint64 { return uint64(f.High)<<32 | uint64(f.Low) }

func winCPU(r *raw) error {
	var idle, kern, user filetime
	ret, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kern)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ret == 0 {
		return err
	}
	// kernel 시간에는 idle이 포함되어 있다(문서에 그렇게 적혀 있다).
	total := kern.u64() + user.u64()
	idleT := idle.u64()
	if total < idleT {
		return fmt.Errorf("시간 값이 뒤집혔습니다")
	}
	r.cpuTotal = total
	r.cpuBusy = total - idleT
	return nil
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func winMem(r *raw) error {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	ret, _, err := procGlobalMemory.Call(uintptr(unsafe.Pointer(&m)))
	if ret == 0 {
		return err
	}
	r.memTotal = m.TotalPhys
	if m.TotalPhys > m.AvailPhys {
		r.memUsed = m.TotalPhys - m.AvailPhys
	}
	// 페이지 파일은 물리 메모리를 포함한 값이라 그대로는 스왑이 아니다.
	// 물리 몫을 빼서 "디스크로 밀린 양"에 가깝게 만든다.
	if m.TotalPageFile > m.TotalPhys {
		r.swapTotal = m.TotalPageFile - m.TotalPhys
		usedPage := m.TotalPageFile - m.AvailPageFile
		if usedPage > r.memUsed {
			r.swapUsed = usedPage - r.memUsed
		}
	}
	return nil
}

func winDisks(r *raw) error {
	buf := make([]uint16, 256)
	ret, _, err := procGetLogicalDrvs.Call(uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if ret == 0 {
		return err
	}
	for _, root := range splitDriveStrings(buf[:ret]) {
		p, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		kind, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(p)))
		// 고정 디스크와 네트워크 드라이브만 본다. CD·USB까지 넣으면 목록이
		// 꽂았다 뺐다 할 때마다 바뀌고, 그 변화는 알림이 될 이유가 없다.
		if uint32(kind) != driveTypeFixed && uint32(kind) != driveTypeRemote {
			continue
		}
		var free, total, totalFree uint64
		ok, _, _ := procGetDiskFree.Call(
			uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&free)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&totalFree)),
		)
		if ok == 0 || total == 0 {
			continue
		}
		fs := "fixed"
		if uint32(kind) == driveTypeRemote {
			fs = "network"
		}
		r.disks = append(r.disks, Disk{
			Mount: strings.TrimSuffix(root, `\`), FS: fs,
			Total: total, Free: free,
		})
	}
	r.disks = sortDisks(r.disks)
	return nil
}

// splitDriveStrings는 "C:\\\x00D:\\\x00\x00" 형태를 문자열 목록으로 만든다.
func splitDriveStrings(buf []uint16) []string {
	out := []string{}
	start := 0
	for i, c := range buf {
		if c != 0 {
			continue
		}
		if i > start {
			out = append(out, windows.UTF16ToString(buf[start:i]))
		}
		start = i + 1
	}
	return out
}

// mibIfRow는 MIB_IFROW다. DWORD와 고정 배열뿐이라 정렬 규칙이 단순하다.
type mibIfRow struct {
	Name            [256]uint16
	Index           uint32
	Type            uint32
	Mtu             uint32
	Speed           uint32
	PhysAddrLen     uint32
	PhysAddr        [8]byte
	AdminStatus     uint32
	OperStatus      uint32
	LastChange      uint32
	InOctets        uint32
	InUcastPkts     uint32
	InNUcastPkts    uint32
	InDiscards      uint32
	InErrors        uint32
	InUnknownProtos uint32
	OutOctets       uint32
	OutUcastPkts    uint32
	OutNUcastPkts   uint32
	OutDiscards     uint32
	OutErrors       uint32
	OutQLen         uint32
	DescrLen        uint32
	Descr           [256]byte
}

const ifTypeLoopback = 24

func winNet(r *raw) error {
	// 크기를 모르므로 한 번 물어보고 그만큼 잡는다.
	var size uint32
	ret, _, _ := procGetIfTable.Call(0, uintptr(unsafe.Pointer(&size)), 0)
	if size == 0 {
		return fmt.Errorf("인터페이스 표 크기를 알 수 없습니다 (코드 %d)", ret)
	}
	buf := make([]byte, size)
	ret, _, err := procGetIfTable.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if ret != 0 {
		return err
	}
	count := *(*uint32)(unsafe.Pointer(&buf[0]))
	rowSize := unsafe.Sizeof(mibIfRow{})

	// 같은 물리 어댑터가 필터 드라이버마다 한 줄씩 나온다
	// (WFP Native MAC Layer, QoS Packet Scheduler …). 그 줄들은 이름이 조금씩
	// 다르지만 **바이트 수가 정확히 같다** — 같은 트래픽을 세고 있기 때문이다.
	// 누적값으로 한 번만 센다. 그러지 않으면 합계가 실제의 두세 배가 되고,
	// 그 그래프는 "네트워크가 포화됐다"는 잘못된 결론으로 이어진다.
	byCounters := map[string]int{} // "rx/tx" → r.nics 안의 자리
	for i := uint32(0); i < count; i++ {
		off := uintptr(4) + uintptr(i)*rowSize
		if off+rowSize > uintptr(len(buf)) {
			break
		}
		row := (*mibIfRow)(unsafe.Pointer(&buf[off]))
		if row.Type == ifTypeLoopback {
			continue
		}
		// 한 바이트도 오간 적 없는 인터페이스는 버린다. 윈도우에는 쓰이지 않는
		// 가상 어댑터가 수십 개씩 있고(WAN Miniport, Teredo, Wi-Fi Direct …),
		// 그것들을 남기면 화면에서 진짜 랜카드를 찾을 수 없다.
		if row.InOctets == 0 && row.OutOctets == 0 {
			continue
		}
		// 이름은 Descr(사람이 읽는 이름)을 쓴다. wszName은
		// \DEVICE\TCPIP_{GUID} 형태라 화면에서 어느 랜카드인지 알 수 없다.
		name := ifDescr(row)
		if name == "" {
			name = fmt.Sprintf("if%d", row.Index)
		}
		nic := NIC{Name: name, RX: uint64(row.InOctets), TX: uint64(row.OutOctets)}

		key := fmt.Sprintf("%d/%d", row.InOctets, row.OutOctets)
		if at, ok := byCounters[key]; ok {
			// 이미 센 트래픽이다. 이름만 더 짧은 쪽으로 바꾼다 —
			// 필터 드라이버 줄은 "어댑터-필터이름-0000"처럼 길고,
			// 짧은 쪽이 어댑터 본래 이름이다.
			if len(name) < len(r.nics[at].Name) {
				r.nics[at].Name = name
			}
			continue
		}
		byCounters[key] = len(r.nics)
		r.nics = append(r.nics, nic)
	}
	return nil
}

// ifDescr은 MIB_IFROW의 설명(ANSI)을 문자열로 만든다.
func ifDescr(row *mibIfRow) string {
	n := int(row.DescrLen)
	if n <= 0 || n > len(row.Descr) {
		return ""
	}
	// DescrLen에 NUL이 포함되어 오는 경우가 있어 잘라 낸다.
	return strings.TrimSpace(strings.TrimRight(string(row.Descr[:n]), "\x00"))
}

func winBoot(r *raw) {
	ms, _, _ := procGetTickCount64.Call()
	if ms > 0 {
		r.bootAt = time.Now().Add(-time.Duration(ms) * time.Millisecond)
	}
}
