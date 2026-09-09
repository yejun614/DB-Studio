package dbx

import (
	"fmt"
	"strings"
)

// ClickHouse 의 큐 엔진.
//
// 이 엔진의 표는 표처럼 보이지만 큐다. 조회하면 **메시지가 큐에서 사라진다** —
// 그래서 ClickHouse 는 직접 조회를 아예 막아 둔다:
//
//	Code: 620. Direct select is not allowed. To enable use setting
//	`stream_like_engine_allow_direct_select`
//
// 데이터 화면이 그 표를 열려고 하면 이 오류가 그대로 사람에게 갔다. 그 문장이
// **나쁜 해결책을 알려 준다**는 것이 문제다 — 적힌 대로 설정을 켜면 화면을 열어
// 볼 때마다 파이프라인의 메시지를 먹는다. 오류가 아니라 설계로 막아야 한다.
//
// 목록을 손으로 들고 있는 이유: system.table_engines 에는 이것을 말해 주는
// 플래그가 없다(supports_ttl·supports_replication 등만 있다). 24.8 에서 확인한
// 목록이고, ClickHouse 가 큐 엔진을 더하면 여기도 더해야 한다. 그때를 위해
// 오류 문장으로도 한 번 더 걸러 둔다(clickhouseDirectSelectBlocked).
var clickhouseStreamEngines = []string{
	"Kafka", "RabbitMQ", "NATS", "FileLog", "S3Queue", "AzureQueue",
}

// clickhouseStreamEngineList는 위 목록을 SQL 의 IN 절에 넣을 꼴로 만든 것이다.
//
// 값을 그 자리에서 문자열로 박는다. 이름이 우리가 적어 둔 상수뿐이라 밖에서
// 들어온 값이 섞일 자리가 없고, IN 절의 원소 수가 바뀌는 자리에 물음표를 세는
// 코드를 두면 목록에 하나를 더할 때 그 셈이 어긋난다.
var clickhouseStreamEngineList = func() string {
	quoted := make([]string, 0, len(clickhouseStreamEngines))
	for _, name := range clickhouseStreamEngines {
		quoted = append(quoted, "'"+name+"'")
	}
	return strings.Join(quoted, ", ")
}()

// clickhouseDirectSelectBlocked는 그 오류가 "큐를 직접 조회했다"인지 본다.
//
// 코드(620)와 설정 이름을 함께 본다. 코드만 보면 ClickHouse 가 그 번호를 다른
// 것에 쓰기 시작할 때 엉뚱한 것을 가리고, 설정 이름만 보면 번역된 메시지에서
// 놓친다.
func clickhouseDirectSelectBlocked(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "stream_like_engine_allow_direct_select") ||
		strings.Contains(msg, "code: 620")
}

// errClickHouseQueueSelect는 사람에게 보일 설명이다.
//
// 설정을 켜라고 적지 않는다. 그것이 이 오류의 원래 문장이 한 일이고, 그대로
// 따르면 화면을 열 때마다 파이프라인이 메시지를 잃는다. 대신 **어디를 봐야
// 하는지**를 적는다 — 큐에서 읽은 것을 쌓아 두는 표가 따로 있고, 사람이 보려던
// 값은 거기 있다.
func errClickHouseQueueSelect(ref TableRef) error {
	return fmt.Errorf("%s 는 큐입니다(Kafka·RabbitMQ 같은 스트림 엔진). "+
		"조회하면 메시지가 큐에서 사라지므로 이 화면에서는 열지 않습니다. "+
		"쌓인 값은 구체화 뷰가 넣어 주는 대상 표에서 보세요 — "+
		"구조 화면에서 이 표를 읽는 구체화 뷰와 그 대상 표를 볼 수 있습니다", ref)
}
