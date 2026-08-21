package api

import (
	"strconv"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/broker"
	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 메시지 브로커(RabbitMQ·Kafka) 관리 API.
//
// 권한은 스토리지와 같은 규칙을 쓴다. 커넥션 등급(monitor/read/write…)이 이미
// "누가 이 대상을 볼 수 있는가"를 정하고 있으므로, 브로커라고 해서 별도의 권한
// 체계를 만들 이유가 없다.
//   - 조회(개요·큐·토픽·그룹): 모니터링 등급 이상
//   - 변경(큐 비우기·지우기, 연결 끊기): data.write 능력
//
// Kafka는 조회 전용이다. 토픽 생성·삭제는 되돌릴 수 없는 조작이고, 카프카는
// 보통 IaC(코드로 관리)로 운영되므로 이 앱에서 실행하지 않는다.

// resolveBroker는 커넥션을 브로커 클라이언트로 만든다.
func (s *Server) resolveBroker(c *fiber.Ctx, level model.Level) (*model.Connection, dbx.Target, error) {
	var t dbx.Target
	id := c.Params("id")
	conn, err := s.st.GetConnection(c.Context(), id)
	if err != nil {
		return nil, t, err
	}
	if !conn.Kind.IsBroker() {
		return nil, t, fiber.NewError(fiber.StatusBadRequest, "이 커넥션은 메시지 브로커가 아닙니다")
	}
	d, err := s.requireLevel(c, conn.ID, level)
	if err != nil {
		return nil, t, err
	}
	if !d.Allowed {
		return nil, t, fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	if !conn.Enabled {
		return nil, t, fiber.NewError(fiber.StatusConflict, "비활성 상태인 커넥션입니다")
	}
	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return nil, t, err
	}
	return conn, dbx.Target{Conn: conn, Secret: secret}, nil
}

func rabbitClient(t dbx.Target) *broker.Rabbit {
	return broker.NewRabbit(broker.ConfigFrom(t.Conn, t.Secret, broker.RabbitDefaultPort))
}

func kafkaClient(t dbx.Target) (*broker.Kafka, error) {
	return broker.NewKafka(broker.ConfigFrom(t.Conn, t.Secret, broker.KafkaDefaultPort))
}

// handleBrokerOverview는 클러스터 개요 한 장이다.
func (s *Server) handleBrokerOverview(c *fiber.Ctx) error {
	conn, t, err := s.resolveBroker(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	var ov *broker.Overview
	switch conn.Kind {
	case model.KindRabbitMQ:
		ov, err = rabbitClient(t).Overview(c.Context())
	case model.KindKafka:
		var cl *broker.Kafka
		cl, err = kafkaClient(t)
		if err == nil {
			defer cl.Close()
			ov, err = cl.Overview(c.Context())
		}
	}
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "broker_unreachable",
			"브로커 상태를 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{
		"overview": ov,
		"kind":     conn.Kind,
		// 화면이 어떤 탭을 그릴지 정하는 근거다. 종류 이름으로 화면에서 분기하면
		// 종류가 늘 때마다 화면을 고쳐야 한다.
		"features": brokerFeatures(conn.Kind),
	})
}

// brokerFeatures는 이 종류에서 쓸 수 있는 기능이다.
func brokerFeatures(kind model.DBKind) fiber.Map {
	switch kind {
	case model.KindRabbitMQ:
		return fiber.Map{"queues": true, "exchanges": true, "connections": true,
			"topics": false, "groups": false, "write": true}
	case model.KindKafka:
		// 쓰기가 false인 이유는 위의 주석에 있다(되돌릴 수 없는 조작이다).
		return fiber.Map{"queues": false, "exchanges": false, "connections": false,
			"topics": true, "groups": true, "write": false}
	}
	return fiber.Map{}
}

// handleBrokerQueues는 RabbitMQ 큐 목록이다.
func (s *Server) handleBrokerQueues(c *fiber.Ctx) error {
	conn, t, err := s.resolveBroker(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if conn.Kind != model.KindRabbitMQ {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 큐가 없습니다")
	}
	limit, _ := strconv.Atoi(c.Query("limit", "0"))
	queues, err := rabbitClient(t).Queues(c.Context(), c.Query("vhost", ""), limit)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "queues_failed",
			"큐 목록을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"queues": queues})
}

// handleBrokerExchanges는 RabbitMQ 익스체인지 목록이다.
func (s *Server) handleBrokerExchanges(c *fiber.Ctx) error {
	conn, t, err := s.resolveBroker(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if conn.Kind != model.KindRabbitMQ {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 익스체인지가 없습니다")
	}
	exchanges, err := rabbitClient(t).Exchanges(c.Context())
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "exchanges_failed",
			"익스체인지 목록을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"exchanges": exchanges})
}

// handleBrokerConnections는 RabbitMQ 클라이언트 연결 목록이다.
func (s *Server) handleBrokerConnections(c *fiber.Ctx) error {
	conn, t, err := s.resolveBroker(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if conn.Kind != model.KindRabbitMQ {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 연결 목록이 없습니다")
	}
	connections, err := rabbitClient(t).Connections(c.Context())
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "connections_failed",
			"연결 목록을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"connections": connections})
}

// handleBrokerTopics는 Kafka 토픽 목록이다.
func (s *Server) handleBrokerTopics(c *fiber.Ctx) error {
	conn, t, err := s.resolveBroker(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if conn.Kind != model.KindKafka {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 토픽이 없습니다")
	}
	cl, err := kafkaClient(t)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "kafka_client_failed",
			"카프카 클라이언트를 만들지 못했습니다", err.Error())
	}
	defer cl.Close()
	limit, _ := strconv.Atoi(c.Query("limit", "0"))
	topics, err := cl.Topics(c.Context(), limit)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "topics_failed",
			"토픽 목록을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"topics": topics})
}

// handleBrokerTopicConfig는 Kafka 토픽 하나의 설정이다.
func (s *Server) handleBrokerTopicConfig(c *fiber.Ctx) error {
	conn, t, err := s.resolveBroker(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if conn.Kind != model.KindKafka {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 토픽 설정이 없습니다")
	}
	cl, err := kafkaClient(t)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "kafka_client_failed",
			"카프카 클라이언트를 만들지 못했습니다", err.Error())
	}
	defer cl.Close()
	config, err := cl.TopicConfig(c.Context(), c.Params("topic"))
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "topic_config_failed",
			"토픽 설정을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"topic": c.Params("topic"), "config": config})
}

// handleBrokerGroups는 Kafka 컨슈머 그룹 목록이다.
func (s *Server) handleBrokerGroups(c *fiber.Ctx) error {
	conn, t, err := s.resolveBroker(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if conn.Kind != model.KindKafka {
		return fiber.NewError(fiber.StatusBadRequest, "이 종류에는 컨슈머 그룹이 없습니다")
	}
	cl, err := kafkaClient(t)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "kafka_client_failed",
			"카프카 클라이언트를 만들지 못했습니다", err.Error())
	}
	defer cl.Close()
	groups, err := cl.Groups(c.Context())
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "groups_failed",
			"컨슈머 그룹 목록을 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"groups": groups})
}

// requireBrokerWrite는 쓰기 능력을 확인한다.
func (s *Server) requireBrokerWrite(c *fiber.Ctx, conn *model.Connection) error {
	if conn.Kind == model.KindKafka {
		return fiber.NewError(fiber.StatusBadRequest,
			"Kafka는 조회 전용입니다. 토픽 생성·삭제는 되돌릴 수 없어 이 앱에서 실행하지 않습니다")
	}
	d, err := s.requireCap(c, conn.ID, model.CapDataWrite)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	return nil
}

// brokerQueueRequest는 큐 조작 입력이다.
type brokerQueueRequest struct {
	VHost string `json:"vhost"`
	Name  string `json:"name"`
}

// handleBrokerPurgeQueue는 큐의 메시지를 모두 버린다.
func (s *Server) handleBrokerPurgeQueue(c *fiber.Ctx) error {
	conn, t, err := s.resolveBroker(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if err := s.requireBrokerWrite(c, conn); err != nil {
		return err
	}
	var req brokerQueueRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if err := rabbitClient(t).PurgeQueue(c.Context(), req.VHost, req.Name); err != nil {
		return failDetail(c, fiber.StatusBadGateway, "purge_failed",
			"큐를 비우지 못했습니다", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "broker.purge_queue", TargetType: "connection", TargetID: conn.ID,
		Detail: map[string]any{"name": conn.Name, "vhost": req.VHost, "queue": req.Name},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// handleBrokerDeleteQueue는 큐를 지운다.
func (s *Server) handleBrokerDeleteQueue(c *fiber.Ctx) error {
	conn, t, err := s.resolveBroker(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if err := s.requireBrokerWrite(c, conn); err != nil {
		return err
	}
	var req brokerQueueRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if err := rabbitClient(t).DeleteQueue(c.Context(), req.VHost, req.Name); err != nil {
		return failDetail(c, fiber.StatusBadGateway, "delete_failed",
			"큐를 지우지 못했습니다", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "broker.delete_queue", TargetType: "connection", TargetID: conn.ID,
		Detail: map[string]any{"name": conn.Name, "vhost": req.VHost, "queue": req.Name},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// handleBrokerCloseConnection은 클라이언트 연결을 끊는다.
func (s *Server) handleBrokerCloseConnection(c *fiber.Ctx) error {
	conn, t, err := s.resolveBroker(c, model.LevelMonitor)
	if err != nil {
		return err
	}
	if err := s.requireBrokerWrite(c, conn); err != nil {
		return err
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if err := rabbitClient(t).CloseConnection(c.Context(), req.Name); err != nil {
		return failDetail(c, fiber.StatusBadGateway, "close_failed",
			"연결을 끊지 못했습니다", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "broker.close_connection", TargetType: "connection", TargetID: conn.ID,
		Detail: map[string]any{"name": conn.Name, "connection": req.Name},
	})
	return c.JSON(fiber.Map{"ok": true})
}
