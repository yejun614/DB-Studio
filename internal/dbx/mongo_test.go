package dbx

import (
	"net/url"
	"strings"
	"testing"

	"dbstudio/internal/model"
)

// mongoURITarget은 URI 조립만 확인하는 대상이다(접속하지 않는다).
// 통합 테스트용 mongoTarget과 이름이 겹치지 않게 따로 둔다.
func mongoURITarget(opts model.Options) Target {
	return Target{
		Conn: &model.Connection{
			Kind: model.KindMongoDB, Host: "db.example.com", Port: 27017,
			DatabaseName: "app", Options: opts,
		},
		Secret: &model.Secret{Username: "svc", Password: "pw"},
	}
}

// TestMongoURIAuthMechanism은 인증 방식이 URI 쿼리로 나가는지 확인한다.
func TestMongoURIAuthMechanism(t *testing.T) {
	a := &mongoAdapter{}
	uri, err := a.uri(mongoURITarget(model.Options{
		"auth_source":               "admin",
		"auth_mechanism":            "SCRAM-SHA-256",
		"auth_mechanism_properties": "SERVICE_NAME:mongodb",
	}))
	if err != nil {
		t.Fatalf("uri: %v", err)
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if got := q.Get("authMechanism"); got != "SCRAM-SHA-256" {
		t.Errorf("authMechanism = %q", got)
	}
	if got := q.Get("authMechanismProperties"); got != "SERVICE_NAME:mongodb" {
		t.Errorf("authMechanismProperties = %q", got)
	}
	if got := q.Get("authSource"); got != "admin" {
		t.Errorf("authSource = %q (다른 옵션을 밀어내면 안 된다)", got)
	}
}

// TestMongoURIWithoutAuthMechanism은 고르지 않았을 때 쿼리에 아무것도 넣지 않는지 확인한다.
// 빈 값을 authMechanism=으로 내보내면 드라이버가 거부한다.
func TestMongoURIWithoutAuthMechanism(t *testing.T) {
	a := &mongoAdapter{}
	uri, err := a.uri(mongoURITarget(model.Options{}))
	if err != nil {
		t.Fatalf("uri: %v", err)
	}
	if strings.Contains(uri, "authMechanism") {
		t.Errorf("고르지 않았는데 쿼리에 들어갔습니다: %s", uri)
	}
}

// TestMongoAuthMechanismNormalizes는 대소문자만 다른 입력을 받아 주는지 확인한다.
func TestMongoAuthMechanismNormalizes(t *testing.T) {
	for _, in := range []string{"scram-sha-256", " SCRAM-SHA-256 ", "Scram-Sha-256"} {
		got, err := mongoAuthMechanism(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != "SCRAM-SHA-256" {
			t.Errorf("%q → %q", in, got)
		}
	}
}

// TestMongoAuthMechanismRejectsUnknown은 알 수 없는 값을 접속 전에 막는지 확인한다.
// 오류 메시지에 가능한 값이 들어 있어야 사용자가 다음에 무엇을 할지 알 수 있다.
func TestMongoAuthMechanismRejectsUnknown(t *testing.T) {
	_, err := mongoAuthMechanism("SCRAM-SHA-999")
	if err == nil {
		t.Fatal("알 수 없는 인증 방식을 통과시켰습니다")
	}
	if !strings.Contains(err.Error(), "SCRAM-SHA-256") {
		t.Errorf("오류 메시지에 가능한 값이 없습니다: %v", err)
	}

	a := &mongoAdapter{}
	if _, err := a.uri(mongoURITarget(model.Options{"auth_mechanism": "nope"})); err == nil {
		t.Error("URI 생성이 잘못된 인증 방식을 통과시켰습니다")
	}
	if err := a.Validate(mongoURITarget(model.Options{"auth_mechanism": "nope"})); err == nil {
		t.Error("Validate가 잘못된 인증 방식을 통과시켰습니다")
	}
}

// TestMongoAuthMechanismHintMatchesValidation은 화면 선택 목록과 검증 목록이
// 같은 값을 보는지 확인한다. 갈라지면 화면에서 고른 값이 저장 시점에 거부된다.
func TestMongoAuthMechanismHintMatchesValidation(t *testing.T) {
	var hint *OptionHint
	for _, h := range optionHints[model.KindMongoDB] {
		if h.Key == "auth_mechanism" {
			cp := h
			hint = &cp
			break
		}
	}
	if hint == nil {
		t.Fatal("MongoDB 옵션에 auth_mechanism 힌트가 없습니다")
	}
	if len(hint.Choices) == 0 {
		t.Fatal("선택 목록이 비었습니다 — 화면이 자유 입력으로 떨어집니다")
	}
	for _, c := range hint.Choices {
		if _, err := mongoAuthMechanism(c); err != nil {
			t.Errorf("화면에서 고를 수 있는 %q를 검증이 거부합니다: %v", c, err)
		}
	}
}

// TestMongoFullURIIgnoresOptions는 전체 URI를 넣었을 때 옵션이 덧붙지 않는지 확인한다.
// 화면의 도움말이 그렇게 약속하고 있고, 사용자가 적은 URI를 고쳐 쓰면 예측할 수 없다.
func TestMongoFullURIIgnoresOptions(t *testing.T) {
	a := &mongoAdapter{}
	raw := "mongodb+srv://user:pw@cluster.example.com/app?authMechanism=MONGODB-AWS"
	uri, err := a.uri(mongoURITarget(model.Options{
		"uri": raw, "auth_mechanism": "SCRAM-SHA-1",
	}))
	if err != nil {
		t.Fatalf("uri: %v", err)
	}
	if uri != raw {
		t.Errorf("전체 URI가 변형되었습니다:\n입력 %s\n출력 %s", raw, uri)
	}
}
