package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/backup"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 백업·복구 API.
//
// 권한은 "그 동작이 실제로 무엇을 하는가"로 정한다.
//
//   - **덤프 만들기**: 구조를 담으면 monitor 등급, 데이터를 담으면 data.read.
//     둘 다 담는 전체 덤프는 둘 다 필요하다. 스키마만 뜨는 것까지 데이터 권한을
//     요구하면 구조 백업이 불가능해지고, 데이터를 담는데 조회 권한이 없어도 되면
//     화면에서 막은 데이터를 파일로 통째로 가져가는 길이 열린다.
//   - **내려받기·미리보기**: 그 백업을 만들 수 있는 권한과 같다. 파일 안에 든 것이
//     같으므로 다른 기준을 쓸 이유가 없다.
//   - **복구**: migrate 등급(구조를 바꾼다) + data.write(데이터를 덮어쓴다).
//     복구는 이 둘을 한꺼번에 하는 동작이므로 어느 한쪽만으로 허용할 수 없다.
//     sql.run을 요구하지 않는 이유는 실행하는 내용이 사용자가 쓴 SQL이 아니라
//     이 앱이 만든 덤프이기 때문이다(외부 파일 업로드 복구를 넣지 않은 이유이기도 하다).

// requireDumpAccess는 덤프 범위에 맞는 권한을 확인하고 대상을 만든다.
func (s *Server) requireDumpAccess(c *fiber.Ctx, connID, scope string) (*backup.Target, error) {
	conn, err := s.st.GetConnection(c.Context(), connID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}

	if scope == backup.ScopeFull || scope == backup.ScopeSchema {
		d, err := s.requireLevel(c, conn.ID, model.LevelMonitor)
		if err != nil {
			return nil, err
		}
		if !d.Allowed {
			return nil, fiber.NewError(fiber.StatusForbidden, d.Reason)
		}
	}
	if scope == backup.ScopeFull || scope == backup.ScopeData {
		d, err := s.requireCap(c, conn.ID, model.CapDataRead)
		if err != nil {
			return nil, err
		}
		if !d.Allowed {
			return nil, fiber.NewError(fiber.StatusForbidden, d.Reason)
		}
	}
	if !conn.Enabled {
		return nil, fiber.NewError(fiber.StatusBadRequest, "비활성화된 커넥션입니다")
	}

	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return nil, err
	}
	return &backup.Target{Conn: conn, Secret: secret}, nil
}

type createBackupRequest struct {
	Scope        string   `json:"scope"`
	Tables       []string `json:"tables"`
	DropIfExists bool     `json:"dropIfExists"`
	Note         string   `json:"note"`
}

func (s *Server) handleCreateBackup(c *fiber.Ctx) error {
	var req createBackupRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if req.Scope == "" {
		req.Scope = backup.ScopeFull
	}
	if !backup.ValidScope(req.Scope) {
		return fail(c, fiber.StatusBadRequest, "invalid_scope", "알 수 없는 덤프 범위입니다")
	}

	target, err := s.requireDumpAccess(c, c.Params("id"), req.Scope)
	if err != nil {
		return err
	}

	id, err := s.backups.StartBackup(c.Context(), backup.StartBackupParams{
		Target: *target,
		Options: backup.Options{
			Scope: req.Scope, Tables: req.Tables,
			DropIfExists: req.DropIfExists, Note: strings.TrimSpace(req.Note),
		},
		Actor: currentUser(c), Trigger: "manual",
	})
	if err != nil {
		return failDetail(c, fiber.StatusBadRequest, "backup_failed",
			"백업을 시작하지 못했습니다", err.Error())
	}

	s.audit(c, store.AuditParams{
		Action: "backup.start", TargetType: "connection", TargetID: target.Conn.ID,
		Detail: map[string]any{
			"name": target.Conn.Name, "backupId": id, "scope": req.Scope,
			"tables": len(req.Tables), "dropIfExists": req.DropIfExists,
		},
	})
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"backupId": id})
}

// handleListBackups는 백업 목록을 반환한다.
//
// 접근 가능한 커넥션의 백업만 보여준다. 목록에는 값이 없지만 "어떤 DB에 무엇이
// 있었는가"는 그 자체로 정보이며, 커넥션 목록을 감추는 규칙과 어긋나면 안 된다.
func (s *Server) handleListBackups(c *fiber.Ctx) error {
	u := currentUser(c)
	conns, err := s.st.ListConnections(c.Context())
	if err != nil {
		return err
	}
	accessible, _, err := s.authz.FilterAccessible(c.Context(), u, conns, model.LevelMonitor)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(accessible))
	for _, conn := range accessible {
		allowed[conn.ID] = true
	}

	items, err := s.st.ListBackups(c.Context(), c.Query("conn"), c.QueryInt("limit", 100))
	if err != nil {
		return err
	}
	out := make([]*store.Backup, 0, len(items))
	for _, b := range items {
		// 커넥션이 지워진 백업은 관리자만 본다. 남은 기록을 아무나 보게 두면
		// 지워진 DB의 존재가 드러난다.
		if b.ConnectionID == "" {
			if !u.Role.CanManageConnections() {
				continue
			}
		} else if !allowed[b.ConnectionID] {
			continue
		}
		b.FileMissing = b.Status == "success" && !s.backups.FileExists(b)
		out = append(out, b)
	}

	restores, err := s.st.ListRestores(c.Context(), c.Query("conn"), 50)
	if err != nil {
		return err
	}
	visibleRestores := make([]*store.Restore, 0, len(restores))
	for _, r := range restores {
		if r.ConnectionID == "" || allowed[r.ConnectionID] {
			visibleRestores = append(visibleRestores, r)
		}
	}

	return c.JSON(fiber.Map{
		"items":     out,
		"restores":  visibleRestores,
		"retention": s.cfg.BackupRetention.String(),
		"maxMB":     s.cfg.BackupMaxMB,
	})
}

// resolveBackup은 백업을 읽고 그것을 만들 수 있는 권한이 있는지 확인한다.
func (s *Server) resolveBackup(c *fiber.Ctx) (*store.Backup, error) {
	b, err := s.st.GetBackup(c.Context(), c.Params("backupId"))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "백업을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	if b.ConnectionID == "" {
		// 커넥션이 지워졌다. 남은 파일에 대한 판단은 관리자만 할 수 있다.
		if !currentUser(c).Role.CanManageConnections() {
			return nil, fiber.NewError(fiber.StatusForbidden, "이 백업에 접근할 권한이 없습니다")
		}
		return b, nil
	}
	if _, err := s.requireDumpAccess(c, b.ConnectionID, b.Scope); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Server) handleGetBackup(c *fiber.Ctx) error {
	b, err := s.resolveBackup(c)
	if err != nil {
		return err
	}
	b.FileMissing = b.Status == "success" && !s.backups.FileExists(b)
	return c.JSON(fiber.Map{"backup": b, "live": s.backups.IsRunning(b.ID)})
}

func (s *Server) handleBackupPreview(c *fiber.Ctx) error {
	b, err := s.resolveBackup(c)
	if err != nil {
		return err
	}
	if b.Status != "success" {
		return fail(c, fiber.StatusBadRequest, "not_ready", "완료된 백업만 볼 수 있습니다")
	}
	text, perr := s.backups.Preview(b, 16*1024)
	if perr != nil {
		return failDetail(c, fiber.StatusBadRequest, "preview_failed",
			"백업을 읽지 못했습니다", perr.Error())
	}
	return c.JSON(fiber.Map{"backup": b, "preview": text})
}

func (s *Server) handleDownloadBackup(c *fiber.Ctx) error {
	b, err := s.resolveBackup(c)
	if err != nil {
		return err
	}
	if b.Status != "success" {
		return fail(c, fiber.StatusBadRequest, "not_ready", "완료된 백업만 내려받을 수 있습니다")
	}

	file, size, oerr := s.backups.OpenForDownload(b)
	if oerr != nil {
		return failDetail(c, fiber.StatusNotFound, "file_missing",
			"백업 파일이 없습니다", oerr.Error())
	}

	// 내려받는 것은 데이터 전체다. 감사 로그에 반드시 남긴다 —
	// "누가 언제 이 DB를 통째로 가져갔는가"는 사고 조사의 첫 질문이다.
	s.audit(c, store.AuditParams{
		Action: "backup.download", TargetType: "connection", TargetID: b.ConnectionID,
		Detail: map[string]any{
			"name": b.ConnectionName, "backupId": b.ID, "scope": b.Scope,
			"bytes": size, "rows": b.RowCount,
		},
	})

	name := fmt.Sprintf("%s-%s%s", safeFileLabel(b.ConnectionName),
		b.StartedAt.UTC().Format("20060102-150405"), dumpDownloadExt(b.Format))
	c.Set(fiber.HeaderContentType, "application/gzip")
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+name+`"`)
	// 스트리밍으로 내보낸다. 파일을 통째로 읽어 메모리에 올리면 큰 백업에서 앱이 죽는다.
	// SendStream이 다 보낸 뒤 닫아 준다.
	return c.SendStream(file, int(size))
}

func dumpDownloadExt(format string) string {
	switch format {
	case backup.FormatJSONL:
		return ".jsonl.gz"
	case backup.FormatRedis:
		return ".redis.gz"
	default:
		return ".sql.gz"
	}
}

// safeFileLabel은 커넥션 이름을 파일 이름에 넣을 수 있게 다듬는다.
// 경로 구분자와 따옴표가 들어가면 Content-Disposition 헤더가 깨진다.
func safeFileLabel(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "backup"
	}
	return out
}

func (s *Server) handleDeleteBackup(c *fiber.Ctx) error {
	b, err := s.resolveBackup(c)
	if err != nil {
		return err
	}
	if s.backups.IsRunning(b.ID) {
		return fail(c, fiber.StatusConflict, "running", "진행 중인 백업은 삭제할 수 없습니다")
	}
	if err := s.backups.Remove(c.Context(), b); err != nil {
		return failDetail(c, fiber.StatusInternalServerError, "delete_failed",
			"백업을 삭제하지 못했습니다", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "backup.delete", TargetType: "connection", TargetID: b.ConnectionID,
		Detail: map[string]any{"name": b.ConnectionName, "backupId": b.ID, "bytes": b.SizeBytes},
	})
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) handleCancelBackup(c *fiber.Ctx) error {
	b, err := s.resolveBackup(c)
	if err != nil {
		return err
	}
	if err := s.backups.Cancel(b.ID); err != nil {
		return fail(c, fiber.StatusBadRequest, "not_running", err.Error())
	}
	return c.JSON(fiber.Map{"ok": true})
}

type restoreRequest struct {
	ConnectionID string `json:"connectionId"`
	// Confirm은 운영 DB에서 커넥션 이름을 그대로 입력했는지 확인하는 값이다.
	Confirm string `json:"confirm"`
}

// handleRestoreBackup은 백업을 대상 커넥션에 되돌린다.
func (s *Server) handleRestoreBackup(c *fiber.Ctx) error {
	b, err := s.resolveBackup(c)
	if err != nil {
		return err
	}
	var req restoreRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	targetID := req.ConnectionID
	if targetID == "" {
		targetID = b.ConnectionID
	}
	if targetID == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "복구할 커넥션을 지정하세요")
	}

	conn, err := s.st.GetConnection(c.Context(), targetID)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "대상 커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	// 복구는 구조와 데이터를 한꺼번에 바꾼다. 두 권한을 모두 요구한다.
	level, err := s.requireLevel(c, conn.ID, model.LevelMigrate)
	if err != nil {
		return err
	}
	if !level.Allowed {
		return fiber.NewError(fiber.StatusForbidden, level.Reason)
	}
	write, err := s.requireCap(c, conn.ID, model.CapDataWrite)
	if err != nil {
		return err
	}
	if !write.Allowed {
		return fiber.NewError(fiber.StatusForbidden, write.Reason)
	}
	if !conn.Enabled {
		return fail(c, fiber.StatusBadRequest, "disabled", "비활성화된 커넥션입니다")
	}

	// 운영 DB에는 확인 문구를 요구한다. 화면도 묻지만, 서버가 다시 확인하지 않으면
	// API를 직접 부르는 경로에서 그 장치가 없는 것과 같다(마이그레이션 실행과 같은 규칙).
	if conn.Environment == model.EnvProd && strings.TrimSpace(req.Confirm) != conn.Name {
		return fail(c, fiber.StatusBadRequest, "confirm_required",
			"운영 DB에 복구하려면 커넥션 이름을 정확히 입력해야 합니다")
	}

	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return err
	}

	id, err := s.backups.StartRestore(c.Context(), backup.StartRestoreParams{
		Backup: b,
		Target: backup.Target{Conn: conn, Secret: secret},
		Actor:  currentUser(c),
	})
	if err != nil {
		return failDetail(c, fiber.StatusBadRequest, "restore_failed",
			"복구를 시작하지 못했습니다", err.Error())
	}

	s.audit(c, store.AuditParams{
		Action: "backup.restore", TargetType: "connection", TargetID: conn.ID,
		Detail: map[string]any{
			"name": conn.Name, "backupId": b.ID, "restoreId": id,
			"sourceConnection": b.ConnectionName, "scope": b.Scope,
			"backupAt": b.StartedAt, "crossConnection": b.ConnectionID != conn.ID,
		},
	})
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"restoreId": id})
}

func (s *Server) handleGetRestore(c *fiber.Ctx) error {
	r, err := s.st.GetRestore(c.Context(), c.Params("restoreId"))
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "복구 기록을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if r.ConnectionID != "" {
		d, lerr := s.requireLevel(c, r.ConnectionID, model.LevelMonitor)
		if lerr != nil {
			return lerr
		}
		if !d.Allowed {
			return fiber.NewError(fiber.StatusForbidden, d.Reason)
		}
	}
	return c.JSON(fiber.Map{"restore": r, "live": s.backups.IsRunning(r.ID)})
}

func (s *Server) handleCancelRestore(c *fiber.Ctx) error {
	r, err := s.st.GetRestore(c.Context(), c.Params("restoreId"))
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "복구 기록을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if r.ConnectionID != "" {
		d, lerr := s.requireLevel(c, r.ConnectionID, model.LevelMigrate)
		if lerr != nil {
			return lerr
		}
		if !d.Allowed {
			return fiber.NewError(fiber.StatusForbidden, d.Reason)
		}
	}
	if err := s.backups.Cancel(r.ID); err != nil {
		return fail(c, fiber.StatusBadRequest, "not_running", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "backup.restore.cancel", TargetType: "connection", TargetID: r.ConnectionID,
		Detail: map[string]any{"name": r.ConnectionName, "restoreId": r.ID},
	})
	return c.JSON(fiber.Map{"ok": true})
}
