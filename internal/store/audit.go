package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/model"
)

// 감사 로그 액션 이름. 문자열 오타를 막기 위해 상수로 고정한다.
const (
	ActionLoginSuccess    = "auth.login.success"
	ActionLoginFailure    = "auth.login.failure"
	ActionLogout          = "auth.logout"
	ActionPasswordChanged = "auth.password.changed"
	ActionProfileUpdated  = "user.profile.updated"
	ActionBootstrap       = "system.bootstrap"
	ActionUserCreated     = "user.created"
	ActionUserUpdated     = "user.updated"
	ActionUserDeleted     = "user.deleted"
	ActionUserPasswordSet = "user.password.reset"
	ActionConnCreated     = "connection.created"
	ActionConnUpdated     = "connection.updated"
	ActionConnDeleted     = "connection.deleted"
	ActionConnTested      = "connection.tested"
	ActionAccessUpdated   = "access.updated"
	ActionServerCreated   = "server.created"
	ActionServerUpdated   = "server.updated"
	ActionServerDeleted   = "server.deleted"
	ActionServerMerged    = "server.merged"

	// 2단계 인증. 성공만이 아니라 실패와 재동기화도 남긴다 —
	// "코드가 계속 안 맞는다"는 문의가 들어왔을 때 볼 곳이 있어야 한다.
	ActionTOTPEnabled       = "auth.totp.enabled"
	ActionTOTPDisabled      = "auth.totp.disabled"
	ActionTOTPReset         = "auth.totp.reset"
	ActionTOTPFailure       = "auth.totp.failure"
	ActionTOTPResync        = "auth.totp.resync"
	ActionTOTPRecoveryUsed  = "auth.totp.recovery.used"
	ActionTOTPRecoveryReset = "auth.totp.recovery.regenerated"
	ActionSecurityUpdated   = "security.policy.updated"
)

// AuditParams는 감사 로그 한 줄의 입력이다.
type AuditParams struct {
	ActorID    string
	ActorName  string
	Action     string
	TargetType string
	TargetID   string
	Detail     map[string]any
	IP         string
	Result     string // "ok" | "denied" | "error". 비어있으면 "ok"
}

// Audit은 감사 로그를 기록한다. 감사 실패가 본 작업을 막지 않도록 에러는 반환만 하고
// 호출부에서 로깅만 하도록 설계했다.
func (s *Store) Audit(ctx context.Context, p AuditParams) error {
	// 리플리카에서 난 일도 기록은 한 곳(마스터)에 모여야 한다. 노드마다 흩어지면
	// "누가 무엇을 했는가"를 보려고 서버를 돌아다녀야 하고, 그 노드가 사라지면
	// 그 기간의 기록도 함께 사라진다.
	if replica, forward := s.auditForwarder(); replica {
		if forward == nil {
			return fmt.Errorf("리플리카에서는 감사 기록을 남길 수 없습니다 (마스터 전달자가 없습니다)")
		}
		return forward(ctx, p)
	}
	detail := "{}"
	if len(p.Detail) > 0 {
		b, err := json.Marshal(p.Detail)
		if err != nil {
			return fmt.Errorf("marshal audit detail: %w", err)
		}
		detail = string(b)
	}
	result := p.Result
	if result == "" {
		result = "ok"
	}
	var actorID any
	if p.ActorID != "" {
		actorID = p.ActorID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_logs
		(at, actor_id, actor_name, action, target_type, target_id, detail, ip, result)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nowString(), actorID, p.ActorName, p.Action, p.TargetType, p.TargetID, detail, p.IP, result)
	if err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}

// AuditFilter는 감사 로그 조회 조건이다.
type AuditFilter struct {
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Since      *time.Time
	Until      *time.Time
	Limit      int
	Offset     int
}

func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]*model.AuditEntry, int, error) {
	where := []string{"1 = 1"}
	args := []any{}
	if f.ActorID != "" {
		where = append(where, "actor_id = ?")
		args = append(args, f.ActorID)
	}
	if f.Action != "" {
		where = append(where, "action LIKE ?")
		args = append(args, f.Action+"%")
	}
	if f.TargetType != "" {
		where = append(where, "target_type = ?")
		args = append(args, f.TargetType)
	}
	if f.TargetID != "" {
		where = append(where, "target_id = ?")
		args = append(args, f.TargetID)
	}
	if f.Since != nil {
		where = append(where, "at >= ?")
		args = append(args, formatTime(*f.Since))
	}
	if f.Until != nil {
		where = append(where, "at <= ?")
		args = append(args, formatTime(*f.Until))
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_logs WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit: %w", err)
	}

	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, at, actor_id, actor_name, action, target_type, target_id, detail, ip, result
		FROM audit_logs WHERE ` + clause + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()

	out := []*model.AuditEntry{}
	for rows.Next() {
		var e model.AuditEntry
		var at, detail string
		var actorID sql.NullString
		if err := rows.Scan(&e.ID, &at, &actorID, &e.ActorName, &e.Action,
			&e.TargetType, &e.TargetID, &detail, &e.IP, &e.Result); err != nil {
			return nil, 0, fmt.Errorf("scan audit: %w", err)
		}
		e.At = parseTime(at)
		e.ActorID = actorID.String
		e.Detail = map[string]any{}
		_ = json.Unmarshal([]byte(detail), &e.Detail)
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate audit: %w", err)
	}
	return out, total, nil
}
