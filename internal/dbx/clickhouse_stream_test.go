package dbx

import (
	"errors"
	"strings"
	"testing"

	"dbstudio/internal/model"
)

// 큐 엔진을 직접 조회했을 때의 오류를 알아봐야 한다.
//
// 왜 필요한가: 그 오류의 원래 문장이 **나쁜 해결책을 알려 준다** —
// "설정을 켜면 된다"고 적혀 있는데, 켜면 화면을 열어 볼 때마다 파이프라인의
// 메시지를 먹는다. 알아보지 못하면 그 문장이 그대로 사람에게 간다.
func TestClickHouseDirectSelectBlocked(t *testing.T) {
	yes := []string{
		// ClickHouse 24.8 이 실제로 돌려준 문장.
		"code: 620, message: Direct select is not allowed. To enable use setting " +
			"`stream_like_engine_allow_direct_select`, but be aware that usually " +
			"the read data is removed from the queue.",
		// 드라이버가 감싸는 모양이 조금 달라도 잡아야 한다.
		"clickhouse [execute]: code: 620, message: Direct select is not allowed",
		// 설정 이름만 있는 경우(번역된 메시지 등).
		"Direct select is not allowed. To enable use setting stream_like_engine_allow_direct_select",
	}
	for _, msg := range yes {
		if !clickhouseDirectSelectBlocked(errors.New(msg)) {
			t.Errorf("알아보지 못했습니다: %s", msg[:60])
		}
	}

	no := []string{
		"code: 60, message: Table appdb.nope does not exist",
		"code: 62, message: Syntax error",
		"driver: bad connection",
		"", // nil 이 아닌 빈 오류
	}
	for _, msg := range no {
		if clickhouseDirectSelectBlocked(errors.New(msg)) {
			t.Errorf("엉뚱한 것을 잡았습니다: %q", msg)
		}
	}
	if clickhouseDirectSelectBlocked(nil) {
		t.Error("nil 을 잡았습니다")
	}
}

// 큐라고 알려 주는 문장에 **설정 이름이 들어가면 안 된다.**
//
// 원래 오류를 그대로 흘리지 않는 것이 이 수정의 요점이다. 설명에 설정 이름을
// 적어 두면 사람이 그것을 검색해 켜게 되고, 그러면 고친 것이 없다.
func TestClickHouseQueueMessageDoesNotSuggestTheSetting(t *testing.T) {
	msg := errClickHouseQueueSelect(TableRef{Namespace: "appdb", Name: "events_queue"}).Error()
	if strings.Contains(msg, "stream_like_engine_allow_direct_select") {
		t.Errorf("설정을 켜라고 알려 주고 있습니다: %s", msg)
	}
	// 어느 표인지와, 대신 어디를 봐야 하는지가 있어야 한다.
	for _, want := range []string{"appdb.events_queue", "큐", "대상 표"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q 가 없습니다: %s", want, msg)
		}
	}
}

// 목록 SQL 이 큐 엔진을 'queue' 로 가른다.
//
// 이름을 하나 더할 때 IN 절이 함께 자라는지도 여기서 본다 — 목록과 SQL 이
// 갈리면 새로 더한 엔진이 표로 보이고, 눌러 보고서야 알게 된다.
func TestClickHouseObjectListSeparatesQueues(t *testing.T) {
	sql, _ := listObjectsSQL(model.KindClickHouse, "appdb", "")
	if !strings.Contains(sql, "'queue'") {
		t.Fatalf("큐를 가르지 않습니다:\n%s", sql)
	}
	for _, engine := range clickhouseStreamEngines {
		if !strings.Contains(sql, "'"+engine+"'") {
			t.Errorf("%s 가 목록 SQL 에 없습니다", engine)
		}
	}
	// 뷰 판정이 큐보다 먼저여야 한다. 큐 엔진 이름에 View 가 붙는 것은 없지만,
	// 순서가 바뀌면 구체화 뷰가 큐로 보일 여지가 생긴다.
	if strings.Index(sql, "'view'") > strings.Index(sql, "'queue'") {
		t.Error("뷰 판정이 큐 판정보다 뒤에 있습니다")
	}
}
