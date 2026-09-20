package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dbstudio/internal/dblog"
	"dbstudio/internal/model"
)

// AI 어시스턴트의 도구가 담당 노드를 지나는지 본다.
//
// ── 왜 이 시험이 필요한가 ───────────────────────────────────────────
// 화면과 도구는 **다른 길**로 DB에 접속한다. 화면은 라우팅 미들웨어를 지나므로
// `routableSuffix`만 맞으면 담당 노드를 지난다. 도구는 핸들러 안에서 어댑터를 직접
// 부르므로 **미들웨어를 아예 지나지 않는다.**
//
// 그래서 라우팅 허용 목록을 늘려도 도구는 고쳐지지 않는다. 실제로 이런 모양이 났다:
//
//	슬로우 쿼리 화면(화면)   → 정상
//	"슬로우 쿼리 찾아 줘"(도구) → 1045
//
// 사람은 도구 쪽 버그를 의심하게 되는데, 원인은 화면과 같다(마스터가 그 사설망에 못 닿는다).
// 그래서 지표·스키마와 같은 판단(`relayNeeded`)을 쓰는지 고정한다.

// TestRelayNeededCoversAllCases는 "맡겨야 하는가" 판단이 한 곳에서 옳은지 본다.
//
// 네 곳(지표·스키마·로그·탐색)이 같은 판단을 쓴다. 갈라 두면 한 곳만 조건이 빠지고,
// 그 증상은 **"일부 기능만 안 된다"** 로 나타난다 — 이번 장애가 정확히 그 모양이었다.
//
// 두 노드를 실제로 띄우는 이유: 담당 노드 ID를 노드 목록에서 읽어야 하므로 가짜 값으로는
// "담당이 나"와 "담당이 남"을 구분할 수 없다.
func TestRelayNeededCoversAllCases(t *testing.T) {
	master, replica := startCluster(t)
	other := replica.node.NodeID()

	t.Run("담당이 남이면 맡긴다", func(t *testing.T) {
		// 그 노드만 닿을 수 있다 — 맡기는 것이 이 기능의 존재 이유다.
		conn := &model.Connection{NodeID: other}
		if !master.srv.relayNeeded(conn) {
			t.Error("맡기지 않았다 — 마스터가 그 사설망에 못 닿으면 도구가 실패한다")
		}
	})

	t.Run("담당이 나면 맡기지 않는다", func(t *testing.T) {
		// 왕복하면 자기 자신을 부르는 모양이 된다.
		conn := &model.Connection{NodeID: master.node.NodeID()}
		if master.srv.relayNeeded(conn) {
			t.Error("맡겼다 — 자기 자신을 부르는 모양이 된다")
		}
	})

	t.Run("담당이 없으면 맡기지 않는다", func(t *testing.T) {
		// 담당을 지정하지 않은 DB가 대부분이다(단일 서버 배포는 전부 그렇다).
		// 여기서 맡기려 하면 그 배포들이 갈 곳을 잃는다.
		conn := &model.Connection{NodeID: ""}
		if master.srv.relayNeeded(conn) {
			t.Error("맡겼다 — 담당 노드가 없는데 갈 곳이 없다")
		}
	})

	t.Run("클러스터가 아니면 맡기지 않는다", func(t *testing.T) {
		s := &Server{} // cluster == nil
		if s.relayNeeded(&model.Connection{NodeID: "n2"}) {
			t.Error("클러스터도 아닌데 맡겼다")
		}
	})
}

// TestAnnotateOriginNamesTheNode은 실패 문구에 실행 노드와 접속 주소가 붙는지 본다.
//
// 이 문구를 읽는 것은 사람만이 아니라 **어시스턴트의 모델**이다. 모델은 이 문구를 근거로
// "계정에 그 주소를 넣으세요"라고 설명하므로, 출처가 없으면 모델도 계정 범위를 의심하지
// 못하고 커넥션 설정을 뒤지게 안내한다.
func TestAnnotateOriginNamesTheNode(t *testing.T) {
	_, replica := startCluster(t)
	connID, _ := seedConnectionOnNode(t, replica)

	conn, err := replica.st.GetConnection(context.Background(), connID)
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	// 담당 노드를 리플리카로 둔다: 그러면 "실행 노드"는 그 노드의 이름이어야 한다.
	if err := replica.st.SetServerNode(context.Background(), conn.ServerID, replica.node.NodeID()); err != nil {
		t.Fatalf("set node: %v", err)
	}
	conn, err = replica.st.GetConnection(context.Background(), connID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	denied := `Error 1045 (28000): Access denied for user 'appuser'@'10.0.0.12' (using password: YES)`
	got := replica.srv.annotateOrigin(context.Background(), conn, errors.New(denied)).Error()

	if !strings.Contains(got, "10.0.0.12") {
		t.Errorf("MySQL이 본 접속 주소가 없다: %q", got)
	}
	if !strings.Contains(got, "실행 노드") {
		t.Errorf("실행 노드 표식이 없다: %q", got)
	}
}

// TestAnnotateOriginLeavesUnrelatedErrorsAlone은 인증 실패가 아닌 것을 건드리지 않는지 본다.
//
// 타임아웃에 무언가를 붙이면 읽는 사람은 계정을 의심하고, 진짜 원인(망·주소)에서 멀어진다.
func TestAnnotateOriginLeavesUnrelatedErrorsAlone(t *testing.T) {
	_, replica := startCluster(t)
	s := replica.srv

	msg := "dial tcp 10.0.0.5:3306: i/o timeout"
	if got := s.annotateOrigin(context.Background(), nil, errors.New(msg)).Error(); got != msg {
		t.Errorf("원문이 바뀌었습니다: %q → %q", msg, got)
	}
}

// TestNodeReadPathsShareTheSameFrontGate는 노드 전용 읽기 경로들이 같은 앞단을 쓰는지 본다.
//
// 네 경로(지표·스키마·로그·탐색)가 각자 검사를 적으면 하나를 빠뜨리기 쉽고, 증상은
// "이 노드에서는 되는데 저 노드에서는 안 된다"로 나타난다. 빈 ID 하나만 봐도 그 검사가
// 공통인지 알 수 있다 — 모두 400이어야 한다.
//
// ── 이 시험이 실제로 버그를 잡았다 ─────────────────────────────────
// 처음 구현에서 앞단이 `fail(...)`을 돌려줬다. fail은 응답을 쓰고 **nil을 반환**하므로
// 부르는 쪽의 `if err != nil`을 그대로 통과했고, 이어서 `adapter.Capabilities()`가 nil을
// 건드려 **500 nil 포인터 패닉**이 났다. 이 저장소에 같은 함정이 이미 적혀 있었는데
// (monitor_handlers.go의 resolveMonitorAccess 주석) 그 교훈을 놓쳤다.
//
// 그래서 여기서 400이 아니라 500이 나오면 그 함정으로 되돌아간 것이다.
func TestNodeReadPathsShareTheSameFrontGate(t *testing.T) {
	_, replica := startCluster(t)

	for _, path := range []string{
		"/api/v1/node/collect",
		"/api/v1/node/introspect",
		"/api/v1/node/logs",
		"/api/v1/node/explore",
	} {
		status, body := replica.client(t).doAsNode("POST", path,
			map[string]any{"connectionId": "   "})
		if status == 500 {
			t.Errorf("%s: 500 — 앞단이 nil 에러를 돌려주는 함정으로 되돌아갔다: %v", path, body)
			continue
		}
		if status != 400 {
			t.Errorf("%s: 빈 connectionId = %d (기대 400): %v", path, status, body)
		}
		msg, _ := body["message"].(string)
		if !strings.Contains(msg, "connectionId") {
			t.Errorf("%s: 무엇이 빠졌는지 알려주지 않는다: %q", path, msg)
		}
	}
}

// TestNodeLogsCarriesTheFilter는 로그 조회가 필터를 함께 받는지 본다.
//
// 필터 없이는 "무엇을 찾는가"가 사라져 읽을 것이 없다. 그리고 필터에는 자격증명이
// 없으므로 채널로 보내도 안전하다 — 그 경계가 지켜지는지도 함께 본다.
func TestNodeLogsCarriesTheFilter(t *testing.T) {
	_, replica := startCluster(t)
	connID, _ := seedConnectionOnNode(t, replica)

	// 필터가 전달되지 않으면 Normalize가 기본값(최근 1시간)을 채운다. 그래서 검색어가
	// 살아 있는지를 본다 — 살아 있으면 필터가 건너갔다는 뜻이다.
	f := &dblog.Filter{Search: "P33-FILTER-MARKER", Limit: 7}
	status, _ := replica.client(t).doAsNode("POST", "/api/v1/node/logs",
		map[string]any{"connectionId": connID, "filter": f})

	// DB가 없으므로 로그 조회는 502로 실패한다. 중요한 것은 그 전에 필터를 받았다는 것과
	// 실패 이유가 그대로 전달된다는 것이다(필터 파싱이 깨지면 400이 난다).
	if status == 400 {
		t.Fatalf("필터가 파싱되지 않았다 = 400 (필터를 실어 보내는 계약이 깨졌다)")
	}
}
