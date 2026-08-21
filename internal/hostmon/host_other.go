//go:build !linux && !windows

package hostmon

import (
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// 리눅스·윈도우가 아닌 유닉스 계열(macOS·BSD)에서는 **읽을 수 있는 것만** 읽는다.
//
// CPU·메모리·네트워크의 누적 카운터는 플랫폼마다 sysctl 이름과 구조체가 다르고,
// 그것을 손으로 옮기다 틀리면 그럴듯한 숫자가 나와 더 나쁘다. 여기서는 어느
// 유닉스에서나 같은 뜻인 것(디스크 사용량, 부팅 시각)만 채우고, 나머지는 못 읽었다고
// 말한다 — DB Studio는 보통 리눅스나 윈도우에서 돌고, 개발용 macOS에서 디스크만
// 보이는 것으로도 충분하다.
func readHost() raw {
	r := raw{
		notes: []string{
			runtime.GOOS + " 에서는 CPU·메모리·네트워크 지표를 읽지 않습니다 (디스크만 표시)",
		},
	}
	readDisksUnix(&r)
	if b, err := unix.SysctlTimeval("kern.boottime"); err == nil {
		r.bootAt = time.Unix(b.Sec, int64(b.Usec)*1000)
	}
	return r
}

// readDisksUnix는 자주 쓰는 마운트 지점을 statfs로 확인한다.
//
// 마운트 목록을 훑지 않는 이유: getfsstat의 구조체가 플랫폼마다 달라 여기서
// 다루면 위 주석의 문제를 그대로 반복하게 된다. 루트와 홈만 봐도 "디스크가
// 차간다"는 신호는 잡힌다.
func readDisksUnix(r *raw) {
	for _, mount := range []string{"/", os.Getenv("HOME")} {
		if mount == "" {
			continue
		}
		var st unix.Statfs_t
		if err := unix.Statfs(mount, &st); err != nil {
			continue
		}
		total := st.Blocks * uint64(st.Bsize)
		if total == 0 {
			continue
		}
		r.disks = append(r.disks, Disk{
			Mount: mount, Total: total, Free: st.Bavail * uint64(st.Bsize),
		})
	}
	r.disks = sortDisks(r.disks)
}
