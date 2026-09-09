package api

import (
	"context"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/docker"
	"dbstudio/internal/model"
)

// DB 컨테이너 관리.
//
// ── 왜 이중 게이트인가 ──────────────────────────────────────────────
// 도커 소켓에 닿는다는 것은 **그 기계에서 무엇이든 할 수 있다**는 뜻이다.
// 특권 컨테이너를 띄워 호스트의 파일 계통을 마운트하면 그것으로 끝이고, 그
// 사이에 아무 경고도 나지 않는다. 그래서 셸 노드와 같은 문을 쓴다.
//
//	1. 서버가 -allow-docker 로 켜져 있어야 한다 (프로세스를 띄우는 사람이 정한다)
//	2. 쓰는 사람이 docker.manage 권한을 가져야 한다 (권한 화면에서 준다)
//
// 둘을 나눠 둔 이유: 권한은 화면에서 몇 번의 클릭으로 바뀌지만, 이 기능이
// 켜지는 순간 이 앱은 호스트를 다룰 수 있는 것이 된다. 그 성격의 변경이
// 클릭으로 일어나서는 안 된다.
//
// ── 왜 상태부터 만드는가 ────────────────────────────────────────────
// 이 기능은 **배치를 손봐야 켜지는** 종류다. 소켓을 마운트했는가, 그 소켓을
// 열 권한이 있는가(이 앱은 nonroot 로 돈다), 데몬이 우리가 말하는 API 버전을
// 받는가. 셋 중 하나만 어긋나도 아무것도 되지 않는데, 그 사실이 "컨테이너
// 만들기가 실패했다"로만 나타나면 어디를 고쳐야 하는지 알 수 없다.
//
// 그래서 첫 화면이 "지금 쓸 수 있는가, 못 쓰면 무엇을 고쳐야 하는가"다.

// dockerClient는 지금 설정으로 데몬에 붙는 클라이언트를 만든다.
//
// 요청마다 만든다. 붙는 비용이 유닉스 소켓 하나를 여는 것뿐이고, 하나를 들고
// 있으면 소켓이 사라졌다가 돌아온 뒤(데몬 재시작) 죽은 커넥션을 물고 있게 된다.
func (s *Server) dockerClient() *docker.Client {
	return docker.New(s.cfg.DockerSocket)
}

// requireDocker는 도커 기능의 이중 게이트다.
//
// 스위치를 권한보다 먼저 본다. 순서가 중요하다 — 권한을 먼저 보면 스위치가
// 꺼진 서버에서 권한 없는 사람에게 "권한이 없습니다"라고 말하게 되고, 그 사람은
// 권한을 달라고 요청한다. 받아도 아무것도 되지 않는다.
func (s *Server) requireDocker(c *fiber.Ctx) error {
	if !s.cfg.AllowDocker {
		return failDetail(c, fiber.StatusForbidden, "docker_disabled",
			"이 서버는 도커 기능이 꺼져 있습니다",
			"-allow-docker (또는 DBSTUDIO_ALLOW_DOCKER=true) 로 켜야 합니다. "+
				"컨테이너로 돌고 있다면 도커 소켓도 함께 넣어 주세요")
	}
	u := currentUser(c)
	if u == nil {
		return fail(c, fiber.StatusUnauthorized, "unauthorized", "로그인이 필요합니다")
	}
	if !u.HasPerm(model.PermDockerManage) {
		s.auditDenied(c, "docker.denied", string(model.PermDockerManage))
		return fail(c, fiber.StatusForbidden, "forbidden",
			model.PermDockerManage.Label()+" 권한이 없습니다")
	}
	return c.Next()
}

// handleDockerStatus는 "지금 도커를 쓸 수 있는가"를 답한다.
//
// 못 쓸 때도 200 이다. 못 쓴다는 것이 이 화면이 보여줄 답이고, 오류로 올리면
// 화면이 "오류 표시"와 "상태 표시"를 따로 만들게 된다 — 그러면 왜 못 쓰는지가
// 오류 토스트로 지나가 버린다.
func (s *Server) handleDockerStatus(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.Context(), s.cfg.DockerTimeout)
	defer cancel()

	st := s.dockerClient().Status(ctx)
	return c.JSON(fiber.Map{
		"status": st,
		// 이 앱이 말하는 API 버전. 데몬이 너무 옛것일 때 화면이 양쪽 값을
		// 함께 보여줄 수 있어야 한다.
		"apiVersion": docker.APIVersion,
	})
}
