package api

import (
	"testing"

	"dbstudio/internal/model"
)

// 브로커 화면은 커넥션의 권한 규칙 위에 얹혀 있다. 등록 화면이 두 종류를
// 볼 수 있는지, 그리고 DB 커넥션에 브로커 API를 부르면 막는지 확인한다.

// TestBrokerKindsAreRegistered는 등록 화면이 두 종류를 볼 수 있는지 본다.
func TestBrokerKindsAreRegistered(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	status, body := c.do("GET", "/api/v1/meta", nil)
	if status != 200 {
		t.Fatalf("meta = %d", status)
	}
	kinds, _ := body["dbKinds"].([]any)
	found := map[string]map[string]any{}
	for _, raw := range kinds {
		k, _ := raw.(map[string]any)
		name, _ := k["kind"].(string)
		found[name] = k
	}
	for _, want := range []string{"rabbitmq", "kafka"} {
		k, ok := found[want]
		if !ok {
			t.Fatalf("%s 종류가 등록 목록에 없습니다", want)
		}
		caps, _ := k["capabilities"].(map[string]any)
		if caps["broker"] != true {
			t.Errorf("%s: broker 능력이 꺼져 있습니다: %v", want, caps)
		}
		// 스키마·마이그레이션은 없어야 한다. 켜져 있으면 화면이 빈 스키마 메뉴를 그린다.
		if caps["introspect"] == true || caps["migrate"] == true || caps["erd"] == true {
			t.Errorf("%s: DB 전용 능력이 켜져 있습니다: %v", want, caps)
		}
		if k["needsDb"] == true {
			t.Errorf("%s: 데이터베이스 이름을 요구하고 있습니다", want)
		}
	}
}

// TestBrokerRejectsNonBroker는 DB 커넥션에 브로커 API를 부르면 막는지 본다.
func TestBrokerRejectsNonBroker(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	conn := storageFixture(t, e, "pg", model.KindPostgres, "http://127.0.0.1:5432")

	status, body := c.do("GET", "/api/v1/connections/"+conn.ID+"/broker", nil)
	if status != 400 {
		t.Fatalf("DB 커넥션의 브로커 조회 = %d (기대 400): %v", status, body)
	}
}
