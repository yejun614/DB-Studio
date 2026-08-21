//go:build linux

package hostmon

import (
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// 리눅스는 필요한 값이 모두 /proc에 있다. 파일을 읽는 것이 전부라 권한 문제도,
// 외부 명령도, 라이브러리도 없다. 컨테이너 안에서는 호스트가 아니라 그 네임스페이스의
// 값이 보이는데, 그것이 이 앱이 실제로 겪는 환경이므로 그대로 쓰는 편이 맞다.

func readHost() raw {
	r := raw{}
	if err := readCPU(&r); err != nil {
		r.notes = append(r.notes, "CPU 사용률을 읽지 못했습니다: "+err.Error())
	}
	if err := readMem(&r); err != nil {
		r.notes = append(r.notes, "메모리 정보를 읽지 못했습니다: "+err.Error())
	}
	if err := readNet(&r); err != nil {
		r.notes = append(r.notes, "네트워크 지표를 읽지 못했습니다: "+err.Error())
	}
	readDisks(&r)
	readLoadAndBoot(&r)
	return r
}

func readCPU(r *raw) error {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return err
	}
	busy, total, ok := parseProcStat(string(b))
	if !ok {
		return errParse
	}
	r.cpuBusy, r.cpuTotal = busy, total
	return nil
}

// parseProcStat은 첫 줄(cpu ...)의 시간 칸을 더한다.
//
// idle과 iowait을 뺀 나머지가 "일한 시간"이다. iowait을 일한 것으로 세면 디스크를
// 기다리는 서버가 CPU 100%로 보이고, 그 그래프는 원인을 반대로 가리킨다.
func parseProcStat(s string) (busy, total uint64, ok bool) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if len(fields) < 5 {
			return 0, 0, false
		}
		var idle uint64
		for i, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		return total - idle, total, true
	}
	return 0, 0, false
}

func readMem(r *raw) error {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return err
	}
	m := parseMeminfo(string(b))
	total, ok := m["MemTotal"]
	if !ok {
		return errParse
	}
	// MemAvailable을 쓴다. free만 보면 캐시가 잡아먹은 메모리를 "사용 중"으로 세어
	// 리눅스 서버는 언제나 메모리가 꽉 찬 것으로 보인다.
	avail, ok := m["MemAvailable"]
	if !ok {
		avail = m["MemFree"] + m["Buffers"] + m["Cached"]
	}
	r.memTotal = total
	if total > avail {
		r.memUsed = total - avail
	}
	if st, ok := m["SwapTotal"]; ok && st > 0 {
		r.swapTotal = st
		if sf, ok := m["SwapFree"]; ok && st > sf {
			r.swapUsed = st - sf
		}
	}
	return nil
}

// parseMeminfo는 "이름: 값 kB" 줄을 바이트 단위 맵으로 만든다.
func parseMeminfo(s string) map[string]uint64 {
	out := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			v *= 1024
		}
		out[name] = v
	}
	return out
}

func readNet(r *raw) error {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return err
	}
	r.nics = parseNetDev(string(b))
	return nil
}

// parseNetDev은 인터페이스별 누적 바이트를 읽는다.
// 루프백은 뺀다 — 자기 자신과 주고받은 것은 "네트워크가 바쁘다"의 근거가 아니다.
func parseNetDev(s string) []NIC {
	out := []NIC{}
	for _, line := range strings.Split(s, "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, NIC{Name: name, RX: rx, TX: tx})
	}
	return out
}

func readDisks(r *raw) {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		r.notes = append(r.notes, "마운트 목록을 읽지 못했습니다: "+err.Error())
		return
	}
	seen := map[string]bool{}
	for _, m := range parseMounts(string(b)) {
		if seen[m.Mount] {
			continue
		}
		var st unix.Statfs_t
		if err := unix.Statfs(m.Mount, &st); err != nil {
			continue
		}
		total := st.Blocks * uint64(st.Bsize)
		if total == 0 {
			continue
		}
		seen[m.Mount] = true
		r.disks = append(r.disks, Disk{
			Mount: m.Mount, FS: m.FS,
			Total: total,
			// Bavail은 **일반 사용자가 쓸 수 있는** 여유다. 루트 예약분(보통 5%)을
			// 여유로 세면 "아직 5% 남았다"가 실제로는 쓸 수 없는 공간이 된다.
			Free: st.Bavail * uint64(st.Bsize),
		})
	}
	r.disks = sortDisks(r.disks)
}

type mountLine struct{ Mount, FS string }

// parseMounts는 실제 저장장치만 남긴다.
//
// tmpfs·proc·cgroup 같은 가상 파일시스템까지 보여주면 목록이 수십 줄이 되고,
// 그중 무엇이 진짜 디스크인지 사람이 골라야 한다.
func parseMounts(s string) []mountLine {
	virtual := map[string]bool{
		"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true,
		"cgroup": true, "cgroup2": true, "securityfs": true, "pstore": true,
		"debugfs": true, "tracefs": true, "mqueue": true, "hugetlbfs": true,
		"fusectl": true, "configfs": true, "bpf": true, "binfmt_misc": true,
		"autofs": true, "ramfs": true, "squashfs": true, "overlay": true, "nsfs": true,
	}
	out := []mountLine{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if virtual[f[2]] {
			continue
		}
		// /proc/mounts는 공백을 \040으로 적는다.
		out = append(out, mountLine{Mount: strings.ReplaceAll(f[1], `\040`, " "), FS: f[2]})
	}
	return out
}

func readLoadAndBoot(r *raw) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				r.load1 = &v
			}
		}
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if secs, err := strconv.ParseFloat(f[0], 64); err == nil {
				r.bootAt = time.Now().Add(-time.Duration(secs * float64(time.Second)))
			}
		}
	}
}
