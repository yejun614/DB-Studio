//go:build windows

package hostmon

import (
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 윈도우에서는 이벤트 로그의 "시스템" 채널을 읽는다.
//
// 옛 API(OpenEventLog/ReadEventLog)를 쓰는 이유: 새 API(EvtQuery)는 XML을 돌려주고
// 그것을 파싱해야 하며, 렌더링 호출이 여러 단계다. 옛 API는 고정 크기 구조체 하나를
// 읽으면 끝이고, 우리가 필요한 것(언제·무엇이·어떤 오류)은 거기 다 있다. 옛 API가
// 못 보는 것은 최신 채널(응용 프로그램별 채널)인데, 디스크·드라이버·서비스·비정상
// 종료는 모두 "시스템" 채널에 남는다.
//
// 메시지 원문(사람이 읽는 문장)은 소스 DLL에서 포맷해야 나온다. 그것까지 하려면
// 레지스트리에서 메시지 파일을 찾아 LoadLibrary/FormatMessage를 해야 하고, 실패
// 경로가 많다. 대신 **소스 이름 + 이벤트 ID + 삽입 문자열**을 그대로 보여준다 —
// 삽입 문자열에 장치 이름이나 서비스 이름 같은 실제 정보가 들어 있다.

var (
	advapi32              = windows.NewLazySystemDLL("advapi32.dll")
	procOpenEventLogW     = advapi32.NewProc("OpenEventLogW")
	procReadEventLogW     = advapi32.NewProc("ReadEventLogW")
	procCloseEventLog     = advapi32.NewProc("CloseEventLog")
	procGetNumberOfEvents = advapi32.NewProc("GetNumberOfEventLogRecords")
	procGetOldestEvent    = advapi32.NewProc("GetOldestEventLogRecord")
)

const (
	eventlogSequentialRead = 0x0001
	eventlogBackwardsRead  = 0x0008
	eventlogErrorType      = 0x0001
	errorHandleEOF         = 38
	errorInsufficientBuf   = 122
	// scanLimit은 한 번에 훑을 기록 수 상한이다. 오래 꺼져 있었다면 밀린 기록이
	// 수만 건일 수 있고, 그것을 모두 훑느라 폴링 한 번이 몇 초씩 걸려서는 안 된다.
	scanLimit = 500
)

// defaultLogPath는 읽을 채널 이름이다. 윈도우에서는 파일 경로가 아니다.
func defaultLogPath() string { return "System" }

// eventLogRecord는 EVENTLOGRECORD의 고정 부분이다.
// DWORD와 WORD뿐이라 정렬이 단순하다(뒤에 문자열들이 이어 붙는다).
type eventLogRecord struct {
	Length              uint32
	Reserved            uint32
	RecordNumber        uint32
	TimeGenerated       uint32
	TimeWritten         uint32
	EventID             uint32
	EventType           uint16
	NumStrings          uint16
	EventCategory       uint16
	ReservedFlags       uint16
	ClosingRecordNumber uint32
	StringOffset        uint32
	UserSidLength       uint32
	UserSidOffset       uint32
	DataLength          uint32
	DataOffset          uint32
}

// Read는 지난번에 본 기록 번호 이후의 오류를 돌려준다.
func (l *OSLog) Read() ([]LogEntry, string) {
	name := l.path
	if name == "" {
		name = defaultLogPath()
		l.path = name
	}
	src, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, "이벤트 로그 이름이 올바르지 않습니다: " + name
	}
	handle, _, callErr := procOpenEventLogW.Call(0, uintptr(unsafe.Pointer(src)))
	if handle == 0 {
		return nil, fmt.Sprintf("이벤트 로그(%s)를 열지 못했습니다: %v", name, callErr)
	}
	defer procCloseEventLog.Call(handle)

	newest := newestRecord(handle)
	if !l.primed {
		// 첫 읽기: 지금 최신 번호만 기억한다.
		l.offset, l.primed = int64(newest), true
		return nil, ""
	}
	if newest == 0 || int64(newest) <= l.offset {
		// 새 기록이 없다. 번호가 작아졌다면 로그를 비운 것이므로 기준을 맞춘다.
		if int64(newest) < l.offset {
			l.offset = int64(newest)
		}
		return nil, ""
	}

	seen := l.offset
	out := []LogEntry{}
	buf := make([]byte, 64*1024)
	scanned := 0

	for len(out) < maxEntries && scanned < scanLimit {
		var read, needed uint32
		ok, _, callErr := procReadEventLogW.Call(handle,
			uintptr(eventlogSequentialRead|eventlogBackwardsRead), 0,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
			uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&needed)))
		if ok == 0 {
			// 버퍼가 작으면 필요한 크기를 알려 준다. 그 한 번만 다시 시도한다.
			if lastErrno(callErr) == errorInsufficientBuf && int(needed) > len(buf) {
				buf = make([]byte, needed)
				continue
			}
			// 나머지는 모두 "더 읽을 것이 없다"로 본다(EOF 포함).
			// 이유를 구분해도 사용자가 할 수 있는 일이 달라지지 않는다.
			break
		}

		off := uint32(0)
		for off+uint32(unsafe.Sizeof(eventLogRecord{})) <= read {
			rec := (*eventLogRecord)(unsafe.Pointer(&buf[off]))
			if rec.Length == 0 || off+rec.Length > read {
				break
			}
			scanned++
			if int64(rec.RecordNumber) <= seen {
				// 지난번에 본 자리까지 왔다. 뒤로 읽고 있으므로 더 볼 것이 없다.
				scanned = scanLimit
				break
			}
			if rec.EventType == eventlogErrorType && len(out) < maxEntries {
				out = append(out, entryFromRecord(buf[off:off+rec.Length], rec, name))
			}
			off += rec.Length
		}
		if off == 0 {
			break
		}
	}

	l.offset = int64(newest)
	return out, ""
}

// lastErrno는 Call이 돌려준 오류에서 코드를 꺼낸다.
func lastErrno(err error) uintptr {
	if e, ok := err.(windows.Errno); ok {
		return uintptr(e)
	}
	return 0
}

// newestRecord는 가장 최근 기록 번호다(가장 오래된 번호 + 개수 - 1).
func newestRecord(handle uintptr) uint32 {
	var count, oldest uint32
	if ok, _, _ := procGetNumberOfEvents.Call(handle, uintptr(unsafe.Pointer(&count))); ok == 0 {
		return 0
	}
	if ok, _, _ := procGetOldestEvent.Call(handle, uintptr(unsafe.Pointer(&oldest))); ok == 0 {
		return 0
	}
	if count == 0 {
		return 0
	}
	return oldest + count - 1
}

// entryFromRecord는 한 기록을 사람이 읽을 한 줄로 만든다.
func entryFromRecord(rec []byte, hdr *eventLogRecord, logName string) LogEntry {
	source, _ := utf16At(rec, uint32(unsafe.Sizeof(eventLogRecord{})))
	parts := insertionStrings(rec, hdr)

	msg := fmt.Sprintf("%s (이벤트 ID %d)", source, hdr.EventID&0xFFFF)
	if len(parts) > 0 {
		msg += ": " + strings.Join(parts, " ")
	}
	return LogEntry{
		At:      time.Unix(int64(hdr.TimeGenerated), 0),
		Source:  logName,
		Message: trimLine(msg),
	}
}

// insertionStrings는 기록에 붙은 삽입 문자열들이다.
//
// 문자열들은 NUL로 구분되어 줄줄이 이어져 있다. 다음 문자열의 시작을 문자 수로
// 계산하지 않고 **읽은 바이트 수**로 받는 이유: UTF-16에는 두 단위로 표현되는 문자가
// 있어 문자 수로 세면 그 뒤가 모두 한 칸씩 밀린다.
func insertionStrings(rec []byte, hdr *eventLogRecord) []string {
	if hdr.NumStrings == 0 || hdr.StringOffset == 0 || int(hdr.StringOffset) >= len(rec) {
		return nil
	}
	out := make([]string, 0, hdr.NumStrings)
	off := hdr.StringOffset
	for i := 0; i < int(hdr.NumStrings) && int(off) < len(rec); i++ {
		s, next := utf16At(rec, off)
		if s != "" {
			out = append(out, s)
		}
		off = next
	}
	return out
}

// utf16At은 버퍼의 주어진 자리에서 NUL까지의 UTF-16 문자열과, 그 다음 자리를 돌려준다.
func utf16At(buf []byte, off uint32) (string, uint32) {
	if int(off)+2 > len(buf) {
		return "", uint32(len(buf))
	}
	units := []uint16{}
	i := int(off)
	for ; i+1 < len(buf); i += 2 {
		u := uint16(buf[i]) | uint16(buf[i+1])<<8
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return strings.TrimSpace(windows.UTF16ToString(units)), uint32(i + 2)
}

// trimLine은 이벤트 메시지에 넣을 만큼만 남긴다.
func trimLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
