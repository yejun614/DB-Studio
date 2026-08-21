// Package buildinfo는 빌드 시점에 주입되는 버전 정보를 보관한다.
//
// 별도 패키지로 둔 이유: 버전을 main에만 두면 API 핸들러가 참조할 수 없고,
// 핸들러 패키지에 두면 릴리스 스크립트의 ldflags 경로가 내부 구현에 묶인다.
// 이 패키지는 다른 것에 의존하지 않으므로 어디서든 읽을 수 있다.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// 아래 세 값은 ldflags로 주입된다.
//
//	-X dbstudio/internal/buildinfo.Version=v1.0.0
//
// 주입하지 않으면 dev로 남고, Commit/Date는 Go가 바이너리에 심는
// VCS 정보에서 채운다(리포지토리 안에서 빌드한 경우).
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info는 화면과 로그에 표시할 빌드 정보다.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	BuildDate string `json:"buildDate,omitempty"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		BuildDate: Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	// ldflags 없이 빌드한 경우에도 커밋을 알 수 있으면 알려준다.
	// 문제 보고에서 "어느 빌드인가"가 가장 먼저 필요한 정보다.
	if info.Commit == "" || info.BuildDate == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if info.Commit == "" {
						info.Commit = shorten(s.Value)
					}
				case "vcs.time":
					if info.BuildDate == "" {
						info.BuildDate = s.Value
					}
				case "vcs.modified":
					if s.Value == "true" && info.Commit != "" {
						info.Commit += "+dirty"
					}
				}
			}
		}
	}
	return info
}

// String은 로그 한 줄에 넣을 수 있는 요약이다.
func (i Info) String() string {
	out := i.Version
	if i.Commit != "" {
		out += " (" + i.Commit + ")"
	}
	return out + " " + i.Platform + " " + i.GoVersion
}

func shorten(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
