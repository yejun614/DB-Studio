package store

import (
	"errors"
	"strings"
)

// IsBusy는 오류가 "메타 DB가 잠시 바쁘다"는 뜻인지 본다(SQLITE_BUSY·SQLITE_LOCKED).
//
// 이 오류만 따로 가려내는 이유: 다른 저장 실패와 달리 **잠시 뒤에 같은 것을 다시 하면
// 된다.** 실패로 돌려보내면 사용자는 방금 한 편집이 사라진 것으로 보고, 실제로 화면도
// 그것을 버린다. 바쁨이라는 것을 알면 부른 쪽이 모아 두었다가 다시 보낼 수 있다.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		// 확장 코드(SQLITE_BUSY_SNAPSHOT 517 등)의 아래 8비트가 기본 코드다.
		switch coded.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database table is locked")
}
