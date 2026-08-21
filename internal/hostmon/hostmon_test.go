package hostmon

import (
	"testing"
	"time"
)

// 이 파일의 시험은 **계산**만 본다(비율, 속도, 임계 판정의 재료).
// 플랫폼 파일의 읽기 자체는 그 OS에서만 돌 수 있으므로, 파싱 함수는 리눅스
// 시험 파일에서 따로 확인한다.

func TestCPUPercentNeedsTwoSamples(t *testing.T) {
	c := New()
	// 첫 표본에는 비교 대상이 없다. 여기서 0을 돌려주면 "방금 켠 서버는 한가하다"는
	// 거짓이 차트 맨 앞에 박히고, 그 뒤의 모든 판단이 그 점에서 시작한다.
	prev := raw{cpuBusy: 100, cpuTotal: 1000}
	if got := cpuPercent(&prev, &raw{cpuBusy: 100, cpuTotal: 1000}); got != nil {
		t.Errorf("변화가 없으면 비율을 만들 수 없다: %v", *got)
	}

	// 절반을 일했으면 50%.
	got := cpuPercent(&prev, &raw{cpuBusy: 150, cpuTotal: 1100})
	if got == nil {
		t.Fatal("두 표본이 있으면 비율이 나와야 한다")
	}
	if *got != 50 {
		t.Errorf("사용률 = %v, 50이어야 한다", *got)
	}

	// 재부팅 등으로 카운터가 되감기면 값을 만들지 않는다.
	if got := cpuPercent(&prev, &raw{cpuBusy: 10, cpuTotal: 20}); got != nil {
		t.Errorf("되감긴 카운터로 비율을 만들었다: %v", *got)
	}
	_ = c
}

func TestNetRatesMatchByName(t *testing.T) {
	prev := []NIC{{Name: "eth0", RX: 1000, TX: 500}, {Name: "eth1", RX: 10, TX: 10}}
	cur := []NIC{{Name: "eth0", RX: 3000, TX: 1500}, {Name: "eth2", RX: 999, TX: 999}}

	rx, tx := netRates(prev, cur, 2)
	if rx == nil || tx == nil {
		t.Fatal("짝이 맞는 인터페이스가 있으면 속도가 나와야 한다")
	}
	// eth0만 짝이 맞는다: (3000-1000)/2 = 1000, (1500-500)/2 = 500.
	// 새로 생긴 eth2의 누적값을 통째로 더하면 랜선 하나 꽂았을 때 그래프가 폭발한다.
	if *rx != 1000 || *tx != 500 {
		t.Errorf("속도 = %v/%v, 1000/500이어야 한다", *rx, *tx)
	}

	// 카운터가 되감긴 구간은 버린다(32비트 카운터는 4GB에서 넘어간다).
	rx, _ = netRates([]NIC{{Name: "eth0", RX: 4_000_000_000}}, []NIC{{Name: "eth0", RX: 5}}, 1)
	if rx == nil || *rx != 0 {
		t.Errorf("되감긴 구간의 속도 = %v, 0이어야 한다", rx)
	}

	// 짝이 하나도 없으면 값을 만들지 않는다.
	if rx, tx := netRates([]NIC{{Name: "a"}}, []NIC{{Name: "b"}}, 1); rx != nil || tx != nil {
		t.Error("짝이 없는데 속도가 나왔다")
	}
}

func TestDiskUsedPercent(t *testing.T) {
	d := Disk{Total: 200, Free: 50}
	if got := d.UsedPercent(); got != 75 {
		t.Errorf("사용률 = %v, 75여야 한다", got)
	}
	// 크기를 모르는 마운트(가상 파일시스템 등)는 0으로 두고 임계 판정에서 빠진다.
	if got := (Disk{}).UsedPercent(); got != 0 {
		t.Errorf("크기가 0인 디스크의 사용률 = %v", got)
	}
}

func TestSampleAlwaysReturnsSomething(t *testing.T) {
	// 무엇을 못 읽든 화면은 떠야 한다. 이 시험은 실제 OS에서 돌며,
	// 못 읽은 항목이 있으면 그 사실이 Notes에 남아 있는지까지 본다.
	c := New()
	s := c.Sample()
	if s == nil {
		t.Fatal("표본이 nil이다")
	}
	if s.At.IsZero() {
		t.Error("표본 시각이 비어 있다")
	}
	if s.Info.CPUs <= 0 {
		t.Error("CPU 개수를 읽지 못했다")
	}
	if s.CPUPercent != nil {
		t.Error("첫 표본에는 CPU 사용률이 없어야 한다")
	}

	// 두 번째 표본에서는 비율이 나온다(플랫폼이 CPU를 지원한다면).
	time.Sleep(30 * time.Millisecond)
	s2 := c.Sample()
	if s2.MemTotal == 0 && len(s2.Notes) == 0 {
		t.Error("메모리를 못 읽었는데 이유가 남지 않았다")
	}
	if s2.CPUPercent != nil && (*s2.CPUPercent < 0 || *s2.CPUPercent > 100) {
		t.Errorf("사용률이 범위를 벗어났다: %v", *s2.CPUPercent)
	}
}

func TestOSLogFirstReadIsSilent(t *testing.T) {
	// 처음 볼 때는 자리만 기억하고 아무것도 만들지 않는다. 그러지 않으면 이 기능을
	// 켠 순간 지난 몇 달의 오류가 이벤트로 쏟아지고, 그 목록은 아무도 보지 않는다.
	l := NewOSLog("", "", 0)
	if !l.Available() {
		t.Skip("이 환경에는 읽을 시스템 로그가 없습니다")
	}
	entries, note := l.Read()
	if len(entries) != 0 {
		t.Errorf("첫 읽기에서 %d건이 나왔다: %v", len(entries), entries[0])
	}
	if note != "" {
		t.Logf("참고: %s", note)
	}
	// 자리는 잡혀 있어야 한다. 그러지 않으면 다음 읽기도 첫 읽기가 된다.
	if l.Offset() == 0 {
		t.Log("위치가 0이다 (로그가 비어 있을 수 있다)")
	}
}

func TestOSLogResumesOnlyForSamePath(t *testing.T) {
	// 저장된 경로와 지금 보려는 경로가 다르면 위치를 이어받지 않는다.
	// 이어받으면 다른 파일의 한가운데를 줄 중간부터 읽는다.
	same := NewOSLog("/var/log/syslog", "/var/log/syslog", 900)
	if same.Offset() != 900 {
		t.Errorf("같은 경로인데 위치를 버렸다: %d", same.Offset())
	}
	other := NewOSLog("/var/log/syslog", "/var/log/messages", 900)
	if other.Offset() != 0 {
		t.Errorf("다른 경로의 위치를 이어받았다: %d", other.Offset())
	}
}

func TestLooksLikeError(t *testing.T) {
	yes := []string{
		"kernel: Out of memory: Killed process 1234 (postgres)",
		"blk_update_request: I/O error, dev sda, sector 1234",
		"systemd[1]: Failed to start PostgreSQL database server.",
		"EXT4-fs error (device sda1): ext4_find_entry:1455",
	}
	for _, line := range yes {
		if !looksLikeError(line) {
			t.Errorf("놓친 오류 줄: %s", line)
		}
	}
	// 평범한 줄은 걸리지 않아야 한다. 여기서 과하게 잡으면 이벤트 표가 로그 사본이 된다.
	no := []string{
		"systemd[1]: Started Daily apt upgrade and clean activities.",
		"CRON[2345]: (root) CMD (/usr/bin/backup.sh)",
		"sshd[999]: Accepted publickey for deploy from 10.0.0.2",
	}
	for _, line := range no {
		if looksLikeError(line) {
			t.Errorf("평범한 줄을 오류로 봤다: %s", line)
		}
	}
}
