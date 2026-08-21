// Package dbstudio는 프론트엔드 정적 자산을 바이너리에 포함시킨다.
// 이 파일이 리포지토리 루트에 있는 이유는 go:embed가 상위 디렉터리를 참조할 수 없기 때문이다.
package dbstudio

import (
	"embed"
	"io/fs"
)

//go:embed all:web
var embedded embed.FS

// WebFS는 web/ 디렉터리를 루트로 하는 파일시스템을 반환한다.
func WebFS() (fs.FS, error) { return fs.Sub(embedded, "web") }
