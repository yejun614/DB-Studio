package api

import (
	"bufio"
	"context"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"dbstudio/internal/applog"
	"dbstudio/internal/docker"
	"dbstudio/internal/store"
)

// 만든 DB 를 다루기 — 실행·중단·다시 시작·지우기, 그리고 로그.
//
// ── 왜 로그가 이 기능의 절반인가 ────────────────────────────────────
// 컨테이너로 띄운 DB 는 뜨지 않을 때 이유를 로그에만 남긴다. 비밀번호가 약한
// 것인지(MS-SQL), 메모리가 모자란 것인지(ClickHouse), 볼륨에 남은 예전
// 데이터와 설정이 어긋난 것인지(PostgreSQL) — 화면에서 그것을 못 보면 사람은
// 결국 서버에 붙어 `docker logs` 를 쳐야 하고, 그러면 이 기능은 있으나 마나다.

// handleStartDBInstance는 멈춰 있는 컨테이너를 시작한다.
func (s *Server) handleStartDBInstance(c *fiber.Ctx) error {
	return s.controlInstance(c, "start")
}

func (s *Server) handleStopDBInstance(c *fiber.Ctx) error {
	return s.controlInstance(c, "stop")
}

func (s *Server) handleRestartDBInstance(c *fiber.Ctx) error {
	return s.controlInstance(c, "restart")
}

// controlInstance는 셋을 한 자리에서 처리한다.
//
// 하나로 묶는 이유: 셋의 차이는 도커에 부르는 함수 하나뿐이고, 그 앞뒤(권한
// 확인·바쁜지 보기·감사 기록·갱신된 줄 돌려주기)는 완전히 같다. 갈라 두면 그
// 앞뒤를 세 벌 적게 되고, 하나를 고칠 때 둘을 빠뜨린다.
func (s *Server) controlInstance(c *fiber.Ctx, op string) error {
	in, err := s.requireDBInstance(c, c.Params("id"))
	if err != nil {
		return err
	}
	// 만드는 중인 것은 건드리지 않는다. 러너가 그 컨테이너를 쓰고 있고,
	// 그 사이에 멈추면 만들기가 "뜨지 못했습니다"로 실패한다 — 사람이 스스로
	// 멈춰 놓고 실패 메시지를 받는 셈이다.
	if in.Status == store.InstanceCreating || s.provisioner().Busy(in.ID) {
		return fail(c, fiber.StatusConflict, "busy",
			"아직 만들고 있습니다. 끝난 뒤에 다시 눌러 주세요")
	}

	ctx, cancel := context.WithTimeout(c.Context(), s.cfg.DockerTimeout+30*time.Second)
	defer cancel()

	var action string
	switch op {
	case "start":
		err, action = s.provisioner().Start(ctx, in), store.ActionDBInstanceStarted
	case "stop":
		err, action = s.provisioner().Stop(ctx, in), store.ActionDBInstanceStopped
	case "restart":
		err, action = s.provisioner().Restart(ctx, in), store.ActionDBInstanceRestarted
	}
	if err != nil {
		if docker.IsNotFound(err) {
			return fail(c, fiber.StatusNotFound, "container_gone",
				"컨테이너가 도커에 없습니다. 목록을 새로 고치면 상태가 맞춰집니다")
		}
		return fail(c, fiber.StatusBadGateway, "docker_failed", err.Error())
	}

	s.audit(c, store.AuditParams{
		Action: action, TargetType: "dbinstance", TargetID: in.ID,
		Detail: map[string]any{"name": in.Name, "kind": in.Kind},
	})

	fresh, err := s.st.GetDBInstance(c.Context(), in.ID)
	if err != nil {
		return err
	}
	// 시작·다시 시작하면 도커가 호스트 포트를 새로 고른다. 등록해 둔 커넥션이
	// 옛 포트를 가리킨 채로 남으면, 우리가 만든 DB 인데 우리가 붙지 못한다.
	s.syncConnectionAddress(c.Context(), fresh)
	return c.JSON(fiber.Map{"instance": fresh})
}

// handleRemoveDBInstance는 컨테이너를 지운다.
//
// ── 두 단계인 이유 ──────────────────────────────────────────────────
// 첫 번째 지우기는 **컨테이너**를 지우고 줄은 남긴다(status=removed). 남기는
// 이유는 실패한 것을 볼 수 있어야 하기 때문이다 — 만들다 실패한 인스턴스의
// 오류 메시지가 그 줄에 있고, 그것을 함께 지우면 왜 실패했는지가 사라진다.
//
// 이미 지워진 줄에 다시 지우기를 부르면 **기록**을 지운다. 지울 컨테이너가 없는
// 상태에서 "지우기"가 뜻할 수 있는 것은 그것뿐이고, 그렇게 두어야 같은 이름을
// 다시 쓸 수 있다(컨테이너 이름은 유일해야 한다).
func (s *Server) handleRemoveDBInstance(c *fiber.Ctx) error {
	in, err := s.requireDBInstance(c, c.Params("id"))
	if err != nil {
		return err
	}
	// 지우기는 **상태를 보지 않는다.** 러너가 실제로 돌고 있을 때만 막는다.
	//
	// creating 인 줄까지 막으면 빠져나올 길이 없어진다: 만드는 도중에 앱이 죽으면
	// 그 줄은 creating 인 채로 남고 러너는 아무것도 하고 있지 않다. 그때 지우기가
	// 그 상태에서 나오는 유일한 문이고, 그 이름을 다시 쓰는 유일한 길이다.
	if s.provisioner().Busy(in.ID) {
		return fail(c, fiber.StatusConflict, "busy",
			"아직 만들고 있습니다. 끝난 뒤에 다시 눌러 주세요")
	}

	// 남은 것이 기록뿐이면 기록을 지운다.
	if in.Status == store.InstanceRemoved && in.ContainerID == "" {
		if err := s.st.DeleteDBInstance(c.Context(), in.ID); err != nil {
			return err
		}
		s.audit(c, store.AuditParams{
			Action: store.ActionDBInstanceRemoved, TargetType: "dbinstance", TargetID: in.ID,
			Detail: map[string]any{"name": in.Name, "record": true},
		})
		return c.JSON(fiber.Map{"deleted": true})
	}

	// 데이터를 남길지는 반드시 요청이 정해야 한다.
	//
	// 기본을 "지운다"로 두면 화면이 그 값을 안 보냈을 때 데이터가 사라진다.
	// 되돌릴 수 없는 쪽이 기본값이 되어서는 안 된다.
	keepData := c.Query("keepData") != "false"

	ctx, cancel := context.WithTimeout(c.Context(), s.cfg.DockerTimeout+30*time.Second)
	defer cancel()
	if err := s.provisioner().Remove(ctx, in, keepData); err != nil {
		return fail(c, fiber.StatusBadGateway, "docker_failed", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionDBInstanceRemoved, TargetType: "dbinstance", TargetID: in.ID,
		Detail: map[string]any{
			"name": in.Name, "kind": in.Kind,
			"volume": in.VolumeName, "dataKept": keepData,
		},
	})
	fresh, err := s.st.GetDBInstance(c.Context(), in.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"instance": fresh})
}

// ---------- 로그 ----------

// dockerLogTail은 처음에 보여줄 줄 수의 상한이다.
//
// 상한을 두는 이유: 몇 달 돌아간 컨테이너의 로그는 수십만 줄이고, 그것을 한
// 번에 보내면 브라우저가 멈춘다. 그리고 그만큼을 읽는 사람도 없다.
const dockerLogTail = 500

// handleDBInstanceLogs는 컨테이너 로그를 SSE 로 흘려보낸다.
//
// ── 왜 스트리밍인가 ─────────────────────────────────────────────────
// 로그를 보는 이유의 대부분이 "지금 무슨 일이 일어나고 있는가"다(뜨는 중,
// 초기화 중, 죽는 중). 되풀어 읽으면 그 사이가 비고, 사람은 새로 고침을
// 누르며 기다린다. 만들기 진행률과 다른 점이 여기다 — 그쪽이 만드는 정보는
// "지금 어디까지 왔는가" 하나라서 덮어쓰면 되지만, 로그는 줄이 쌓인다.
func (s *Server) handleDBInstanceLogs(c *fiber.Ctx) error {
	in, err := s.requireDBInstance(c, c.Params("id"))
	if err != nil {
		return err
	}
	if in.ContainerID == "" {
		return fail(c, fiber.StatusConflict, "no_container",
			"컨테이너가 없어 로그를 읽을 수 없습니다")
	}

	tail, _ := strconv.Atoi(c.Query("tail"))
	if tail <= 0 || tail > dockerLogTail {
		tail = dockerLogTail
	}
	follow := c.Query("follow") != "false"

	// 요청의 컨텍스트를 쓸 수 없다.
	//
	// 스트림 라이터는 핸들러가 반환한 **뒤에** 돌고, 그때 c.Context() 는 이미
	// 취소되어 있다. 그것을 넘기면 로그가 열리자마자 닫힌다.
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := s.provisioner().Logs(ctx, in, docker.LogOptions{
		Tail: tail, Follow: follow, Timestamps: true,
	})
	if err != nil {
		cancel()
		if docker.IsNotFound(err) {
			return fail(c, fiber.StatusNotFound, "container_gone",
				"컨테이너가 도커에 없습니다")
		}
		return fail(c, fiber.StatusBadGateway, "docker_failed", err.Error())
	}

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	conn := s.streamConn(c)
	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		// 스트림 라이터는 Fiber 의 recover 밖에서 돈다.
		defer applog.Recover("docker.logs.stream")
		defer cancel()
		defer stream.Close()

		out := &sseWriter{w: w, conn: conn}
		// 도커에서 읽는 일을 따로 떼어 낸다.
		//
		// 왜 필요한가: 조용한 컨테이너의 로그를 따라가는 동안 읽기는 막혀 있고,
		// 그 자리에서는 브라우저가 창을 닫은 것을 알 수 없다. 그러면 아무도 보지
		// 않는 스트림이 도커 쪽에 계속 열려 있게 된다. 갈라 두면 쓰기 실패로
		// 알아채고 스트림을 닫아 읽기까지 함께 풀 수 있다.
		lines := make(chan docker.LogLine, 256)
		done := make(chan struct{})
		go func() {
			defer close(lines)
			for line := range stream.Lines() {
				select {
				case lines <- line:
				case <-done:
					return
				}
			}
		}()
		defer close(done)

		// 하트비트: 프록시와 브라우저가 조용한 연결을 끊는 것을 막는다.
		// 막 뜬 DB 는 한동안 아무 줄도 남기지 않는 구간이 흔하다.
		ticker := time.NewTicker(streamPingEvery)
		defer ticker.Stop()

		for {
			select {
			case line, ok := <-lines:
				if !ok {
					// 따라가지 않는 요청이거나 컨테이너가 멈췄다.
					_ = out.send("end", fiber.Map{"reason": "로그가 끝났습니다"})
					return
				}
				if out.send("log", line) != nil {
					return
				}
			case <-ticker.C:
				if out.send("ping", fiber.Map{"t": time.Now().Unix()}) != nil {
					return
				}
			}
		}
	}))
	return nil
}
