package api

import (
	"strings"
	"testing"

	"dbstudio/internal/model"
)

// 대화에 대상 DB를 지정했는데 모델이 "어느 DB를 볼까요?"라고 되묻는 것은
// 사용자 입장에서 앱이 고장 난 것으로 보인다 — 이미 고른 것을 다시 묻기 때문이다.
// 그 사실은 시스템 프롬프트로만 전달되므로 여기서 고정한다.
func TestSessionPromptCarriesTargetConnection(t *testing.T) {
	conn := &model.Connection{
		Name: "운영 MySQL", Kind: model.KindMySQL, Environment: model.EnvProd,
	}
	got := sessionPrompt(conn)

	for _, want := range []string{"운영 MySQL", string(model.KindMySQL), "운영"} {
		if !strings.Contains(got, want) {
			t.Errorf("프롬프트에 %q 가 없다:\n%s", want, got)
		}
	}
	// 기본 지침이 사라지면 안 된다. 덧붙이는 것이지 대체하는 것이 아니다.
	if !strings.Contains(got, "DB Studio의 어시스턴트") {
		t.Error("기본 지침이 사라졌다")
	}
	// "다시 묻지 마세요"가 이 문단의 목적이다.
	if !strings.Contains(got, "다시 묻지") {
		t.Errorf("되묻지 말라는 지시가 없다:\n%s", got)
	}
}

// 대상 DB가 없는 대화도 정상이다. 그때는 기본 지침만 쓴다.
func TestSessionPromptWithoutConnection(t *testing.T) {
	if got := sessionPrompt(nil); got != systemPrompt {
		t.Errorf("대상 DB 없는 프롬프트가 기본과 다르다:\n%s", got)
	}
}

// 개발 환경은 운영으로 표시되면 안 된다. 모델이 위험도를 그 값으로 판단한다.
func TestSessionPromptEnvironmentLabel(t *testing.T) {
	dev := sessionPrompt(&model.Connection{
		Name: "dev", Kind: model.KindPostgres, Environment: model.EnvDev,
	})
	if !strings.Contains(dev, "환경: 개발") {
		t.Errorf("개발 환경 표기가 없다:\n%s", dev)
	}
	if strings.Contains(dev, "환경: 운영") {
		t.Errorf("개발 DB가 운영으로 표기됐다:\n%s", dev)
	}
}
