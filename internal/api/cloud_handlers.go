package api

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/cloud"
	"dbstudio/internal/store"
)

// 클라우드 연결 화면의 API (Cloudflare D1 동기화, R2·S3 백업).
//
// 슈퍼 어드민만 다룬다: 여기서 고르는 것은 "이 앱의 데이터 전체를 어디에 두는가"이고,
// 가져오기·복구는 이 서버의 데이터를 통째로 바꾼다. 토큰 하나로 메타 DB 전체를 읽을 수
// 있는 곳을 정하는 일이기도 하다.

// SetCloud는 클라우드 서비스를 붙인다. 붙이지 않으면 화면은 "꺼져 있음"으로 답한다.
func (s *Server) SetCloud(c *cloud.Service) { s.cloud = c }

// CloseAllERDRooms는 열린 ERD 방을 모두 닫는다(메타 DB를 통째로 바꾼 뒤).
func (s *Server) CloseAllERDRooms(reason string) { s.erdHub.CloseAll(reason) }

func (s *Server) cloudOff(c *fiber.Ctx) error {
	return fail(c, fiber.StatusServiceUnavailable, "cloud_off", "클라우드 연결 기능이 꺼져 있습니다")
}

func (s *Server) handleGetCloud(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	d1, err := s.st.D1Settings(c.Context())
	if err != nil {
		return err
	}
	obj, err := s.st.ObjectSettings(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"d1":     d1.Masked(),
		"object": obj.Masked(),
		"status": s.cloud.Status(c.Context()),
		"limits": fiber.Map{
			"intervalMin": store.D1IntervalMin, "intervalMax": store.D1IntervalMax,
		},
	})
}

// d1Payload는 D1 설정 화면이 보내는 값이다. 토큰을 포인터로 받는 이유는 알림 설정과
// 같다 — 마스킹된 값을 보여 주므로, 바꾸지 않으면 칸이 아예 오지 않는다.
type d1Payload struct {
	Enabled     bool    `json:"enabled"`
	AccountID   string  `json:"accountId"`
	DatabaseID  string  `json:"databaseId"`
	APIToken    *string `json:"apiToken"`
	IntervalSec int     `json:"intervalSec"`
}

// cloudID는 계정 ID·데이터베이스 ID 꼴이다(16진·UUID). 주소에 그대로 들어가므로 좁게 받는다.
var cloudID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func (s *Server) handlePutCloudD1(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	var in d1Payload
	if err := c.BodyParser(&in); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	cur, err := s.st.D1Settings(c.Context())
	if err != nil {
		return err
	}
	next := *cur
	next.Enabled = in.Enabled
	next.AccountID = strings.TrimSpace(in.AccountID)
	next.DatabaseID = strings.TrimSpace(in.DatabaseID)
	if in.APIToken != nil {
		next.APIToken = strings.TrimSpace(*in.APIToken)
	}
	next.IntervalSec = in.IntervalSec
	if next.IntervalSec <= 0 {
		next.IntervalSec = store.D1IntervalDefault
	}
	if next.IntervalSec < store.D1IntervalMin || next.IntervalSec > store.D1IntervalMax {
		return fail(c, fiber.StatusBadRequest, "invalid_settings", "동기화 주기는 10초에서 3600초 사이여야 합니다")
	}
	for _, v := range []string{next.AccountID, next.DatabaseID} {
		if v != "" && !cloudID.MatchString(v) {
			return fail(c, fiber.StatusBadRequest, "invalid_settings", "계정 ID·데이터베이스 ID 형식이 올바르지 않습니다")
		}
	}
	if next.Enabled && !next.Ready() {
		return fail(c, fiber.StatusBadRequest, "invalid_settings",
			"계정 ID·데이터베이스 ID·API 토큰이 있어야 동기화를 켤 수 있습니다")
	}

	u := currentUser(c)
	if err := s.st.SaveD1Settings(c.Context(), next, u.ID); err != nil {
		return err
	}
	// 다른 D1을 가리키게 됐으면 "어디까지 보냈는가"를 잊는다. 그 지점은 옛 D1의 것이다.
	if cur.AccountID != next.AccountID || cur.DatabaseID != next.DatabaseID {
		s.cloud.ResetD1(c.Context())
	} else {
		s.cloud.Kick()
	}
	s.audit(c, store.AuditParams{
		Action: "cloud.d1.updated", TargetType: "settings", TargetID: store.SettingCloudD1,
		// 토큰은 감사 로그에도 남기지 않는다.
		Detail: map[string]any{
			"enabled": next.Enabled, "accountId": next.AccountID, "databaseId": next.DatabaseID,
			"intervalSec": next.IntervalSec, "hasToken": next.APIToken != "",
		},
	})
	saved, err := s.st.D1Settings(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"d1": saved.Masked(), "status": s.cloud.Status(c.Context())})
}

func (s *Server) handleTestCloudD1(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	info, err := s.cloud.TestD1(c.Context())
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "d1_failed", err.Error())
	}
	return c.JSON(fiber.Map{"remote": info})
}

func (s *Server) handleSyncCloudD1(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	s.cloud.Kick()
	return c.JSON(fiber.Map{"ok": true})
}

// handlePushCloudD1은 이 서버의 데이터로 D1을 덮어쓴다(이 서버가 주인이 된다).
func (s *Server) handlePushCloudD1(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	job, err := s.cloud.StartPush()
	if err != nil {
		return s.cloudJobError(c, err)
	}
	s.audit(c, store.AuditParams{Action: "cloud.d1.push", TargetType: "settings", TargetID: store.SettingCloudD1})
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"job": job})
}

// handlePullCloudD1은 D1의 데이터로 이 서버의 데이터를 덮어쓴다.
func (s *Server) handlePullCloudD1(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	// 감사 기록은 덮어쓴 **뒤에** 남긴다. 감사 로그 표도 받아 온 것으로 바뀌므로, 먼저
	// 남기면 "누가 가져왔는가"가 사라진다. 그 시점에는 요청이 끝나 있으므로 값으로 넘긴다.
	job, err := s.cloud.StartPull(s.auditLater(c, store.AuditParams{
		Action: "cloud.d1.pull", TargetType: "settings", TargetID: store.SettingCloudD1,
	}))
	if err != nil {
		return s.cloudJobError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"job": job})
}

// objectPayload는 오브젝트 스토리지 설정 화면이 보내는 값이다.
type objectPayload struct {
	Provider        string  `json:"provider"`
	AccountID       string  `json:"accountId"`
	Endpoint        string  `json:"endpoint"`
	Region          string  `json:"region"`
	Bucket          string  `json:"bucket"`
	Prefix          string  `json:"prefix"`
	AccessKey       string  `json:"accessKey"`
	SecretKey       *string `json:"secretKey"`
	VirtualHost     bool    `json:"virtualHost"`
	Encrypt         bool    `json:"encrypt"`
	ScheduleEnabled bool    `json:"scheduleEnabled"`
	EveryHours      int     `json:"everyHours"`
	At              string  `json:"at"`
	Keep            int     `json:"keep"`
}

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func (s *Server) handlePutCloudObject(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	var in objectPayload
	if err := c.BodyParser(&in); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	cur, err := s.st.ObjectSettings(c.Context())
	if err != nil {
		return err
	}
	next := *cur
	next.Provider = strings.TrimSpace(in.Provider)
	next.AccountID = strings.TrimSpace(in.AccountID)
	next.Endpoint = strings.TrimRight(strings.TrimSpace(in.Endpoint), "/")
	next.Region = strings.TrimSpace(in.Region)
	next.Bucket = strings.TrimSpace(in.Bucket)
	next.Prefix = strings.Trim(strings.TrimSpace(in.Prefix), "/")
	if next.Prefix != "" {
		next.Prefix += "/"
	}
	next.AccessKey = strings.TrimSpace(in.AccessKey)
	if in.SecretKey != nil {
		next.SecretKey = strings.TrimSpace(*in.SecretKey)
	}
	next.VirtualHost = in.VirtualHost
	next.Encrypt = in.Encrypt
	next.ScheduleEnabled = in.ScheduleEnabled
	next.EveryHours = in.EveryHours
	next.At = strings.TrimSpace(in.At)
	next.Keep = in.Keep

	switch next.Provider {
	case "r2", "s3", "custom":
	default:
		return fail(c, fiber.StatusBadRequest, "invalid_settings", "스토리지 종류는 r2·s3·custom 중 하나입니다")
	}
	if next.AccountID != "" && !cloudID.MatchString(next.AccountID) {
		return fail(c, fiber.StatusBadRequest, "invalid_settings", "계정 ID 형식이 올바르지 않습니다")
	}
	if next.Endpoint != "" && !strings.HasPrefix(next.Endpoint, "https://") && !strings.HasPrefix(next.Endpoint, "http://") {
		return fail(c, fiber.StatusBadRequest, "invalid_settings", "엔드포인트는 https:// 또는 http:// 로 시작해야 합니다")
	}
	if strings.ContainsAny(next.Bucket, "/ ") {
		return fail(c, fiber.StatusBadRequest, "invalid_settings", "버킷 이름에 / 나 공백을 쓸 수 없습니다")
	}
	if next.EveryHours < 1 || next.EveryHours > 720 {
		return fail(c, fiber.StatusBadRequest, "invalid_settings", "백업 주기는 1시간에서 720시간(30일) 사이여야 합니다")
	}
	if !hhmm.MatchString(next.At) {
		return fail(c, fiber.StatusBadRequest, "invalid_settings", "백업 시각은 HH:MM 형식이어야 합니다 (예: 03:00)")
	}
	if next.Keep < 0 || next.Keep > 1000 {
		return fail(c, fiber.StatusBadRequest, "invalid_settings", "보존 개수는 0에서 1000 사이여야 합니다")
	}
	if next.ScheduleEnabled && !next.Ready() {
		return fail(c, fiber.StatusBadRequest, "invalid_settings",
			"접속 정보(버킷·키·계정 ID 또는 엔드포인트)를 모두 채워야 정기 백업을 켤 수 있습니다")
	}
	if _, err := cloud.NewObjectStore(next); err != nil && next.Ready() {
		return fail(c, fiber.StatusBadRequest, "invalid_settings", err.Error())
	}

	u := currentUser(c)
	if err := s.st.SaveObjectSettings(c.Context(), next, u.ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "cloud.object.updated", TargetType: "settings", TargetID: store.SettingCloudObject,
		Detail: map[string]any{
			"provider": next.Provider, "bucket": next.Bucket, "prefix": next.Prefix,
			"encrypt": next.Encrypt, "scheduleEnabled": next.ScheduleEnabled,
			"everyHours": next.EveryHours, "at": next.At, "keep": next.Keep,
			"hasSecret": next.SecretKey != "",
		},
	})
	saved, err := s.st.ObjectSettings(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"object": saved.Masked(), "status": s.cloud.Status(c.Context())})
}

func (s *Server) handleTestCloudObject(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	where, count, err := s.cloud.TestObject(c.Context())
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "object_failed", err.Error())
	}
	return c.JSON(fiber.Map{"location": where, "backups": count})
}

func (s *Server) handleListCloudBackups(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	list, err := s.cloud.ListBackups(c.Context())
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "object_failed", err.Error())
	}
	return c.JSON(fiber.Map{"backups": list})
}

func (s *Server) handleCreateCloudBackup(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	job, err := s.cloud.StartBackup()
	if err != nil {
		return s.cloudJobError(c, err)
	}
	s.audit(c, store.AuditParams{Action: "cloud.object.backup", TargetType: "settings", TargetID: store.SettingCloudObject})
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"job": job})
}

func (s *Server) handleRestoreCloudBackup(c *fiber.Ctx) error {
	if s.cloud == nil {
		return s.cloudOff(c)
	}
	var in struct {
		Key string `json:"key"`
	}
	if err := c.BodyParser(&in); err != nil || strings.TrimSpace(in.Key) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "복구할 백업을 고르세요")
	}
	job, err := s.cloud.StartRestore(in.Key, s.auditLater(c, store.AuditParams{
		Action: "cloud.object.restore", TargetType: "settings", TargetID: store.SettingCloudObject,
		Detail: map[string]any{"key": in.Key},
	}))
	if err != nil {
		return s.cloudJobError(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"job": job})
}

// auditLater는 요청이 끝난 뒤에 남길 감사 기록을 만든다(행위자·IP를 지금 값으로 잡아 둔다).
func (s *Server) auditLater(c *fiber.Ctx, p store.AuditParams) func(context.Context) {
	u := currentUser(c)
	actorID, actorName := "", ""
	if u != nil {
		actorID, actorName = u.ID, u.Username
	}
	ip := clientIP(c)
	return func(ctx context.Context) { s.auditDirect(ctx, actorID, actorName, ip, p) }
}

func (s *Server) cloudJobError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, cloud.ErrBusy):
		return fail(c, fiber.StatusConflict, "cloud_busy", err.Error())
	case errors.Is(err, cloud.ErrReplica):
		return fail(c, fiber.StatusConflict, "cloud_replica", err.Error())
	}
	return fail(c, fiber.StatusBadRequest, "cloud_failed", err.Error())
}
