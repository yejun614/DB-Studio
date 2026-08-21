//go:build linux

package hostmon

import "testing"

// /proc 형식 파싱. 실제 파일 대신 문자열로 시험한다 —
// 컨테이너·CI마다 내용이 다르고, 여기서 확인할 것은 형식 해석이지 그 값이 아니다.

func TestParseProcStat(t *testing.T) {
	// idle(4번째)과 iowait(5번째)은 "일한 시간"이 아니다.
	// iowait을 일한 것으로 세면 디스크를 기다리는 서버가 CPU 100%로 보인다.
	busy, total, ok := parseProcStat("cpu  10 0 20 100 30 0 0 0 0 0\ncpu0 1 2 3 4 5\n")
	if !ok {
		t.Fatal("첫 줄을 해석하지 못했다")
	}
	if total != 160 {
		t.Errorf("전체 = %d, 160이어야 한다", total)
	}
	if busy != 30 {
		t.Errorf("사용 = %d, 30이어야 한다(idle 100 + iowait 30 제외)", busy)
	}
	if _, _, ok := parseProcStat("intr 1 2 3\n"); ok {
		t.Error("cpu 줄이 없는데 성공으로 판정했다")
	}
}

func TestParseMeminfo(t *testing.T) {
	m := parseMeminfo("MemTotal:       16384 kB\nMemAvailable:    8192 kB\nHugePages_Total:     0\n")
	if m["MemTotal"] != 16384*1024 {
		t.Errorf("MemTotal = %d, 바이트로 바뀌어야 한다", m["MemTotal"])
	}
	// 단위가 없는 줄은 그대로 둔다.
	if m["HugePages_Total"] != 0 {
		t.Errorf("HugePages_Total = %d", m["HugePages_Total"])
	}
}

func TestParseNetDev(t *testing.T) {
	out := parseNetDev(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets
    lo:  123456    100    0    0    0     0          0         0   123456    100
  eth0: 1000000   2000    0    0    0     0          0         0  2000000   3000
`)
	// 루프백은 뺀다 — 자기 자신과 주고받은 것은 "네트워크가 바쁘다"의 근거가 아니다.
	if len(out) != 1 || out[0].Name != "eth0" {
		t.Fatalf("인터페이스 = %+v", out)
	}
	if out[0].RX != 1000000 || out[0].TX != 2000000 {
		t.Errorf("누적값 = %d/%d", out[0].RX, out[0].TX)
	}
}

func TestParseMountsDropsVirtual(t *testing.T) {
	out := parseMounts(`/dev/sda1 / ext4 rw 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
/dev/sdb1 /mnt/data\040backup xfs rw 0 0
`)
	if len(out) != 2 {
		t.Fatalf("마운트 = %+v (가상 파일시스템이 남았다)", out)
	}
	// /proc/mounts는 공백을 \040으로 적는다. 그대로 두면 statfs가 실패한다.
	if out[1].Mount != "/mnt/data backup" {
		t.Errorf("마운트 지점 = %q", out[1].Mount)
	}
}
