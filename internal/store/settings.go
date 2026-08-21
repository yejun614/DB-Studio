package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// 앱 전역 설정. 키·값 한 테이블에 모아 두고, 타입 있는 접근자만 밖으로 노출한다.
//
// 문자열 키를 핸들러가 직접 쓰지 못하게 하는 이유: 오타 난 키는 조용히 기본값으로
// 읽히고, 그러면 "설정을 켰는데 안 켜진" 상태가 아무 오류 없이 만들어진다.
const (
	// SettingTOTPRequired는 모든 사용자에게 2단계 인증을 의무화할지다.
	SettingTOTPRequired = "security.totp.required"
	// SettingClockOffset은 학습된 내부 시계 보정값(초)이다. 앱이 스스로 갱신한다.
	SettingClockOffset = "clock.offset_seconds"
)

// SecurityPolicy는 슈퍼 어드민이 정하는 전역 보안 설정이다.
type SecurityPolicy struct {
	// TOTPRequired가 참이면 모든 사용자가 2단계 인증을 등록해야 한다.
	// 거짓이면 각자 자율적으로 켠다(기본값).
	TOTPRequired bool       `json:"totpRequired"`
	UpdatedAt    *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy    string     `json:"updatedBy,omitempty"`
}

// GetSetting은 설정 하나를 읽는다. 없으면 빈 문자열과 ErrNotFound를 돌려준다.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get setting %s: %w", key, err)
	}
	return v, nil
}

// SetSetting은 설정 하나를 쓴다. actorID가 비어 있으면 앱이 스스로 갱신한 값이다.
func (s *Store) SetSetting(ctx context.Context, key, value, actorID string) error {
	var by any
	if actorID != "" {
		by = actorID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO app_settings (key, value, updated_at, updated_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value,
			updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		key, value, nowString(), by)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	s.invalidatePolicy()
	return nil
}

// SecurityPolicy는 전역 보안 설정을 반환한다.
//
// 캐시하는 이유: 이 값은 **모든 인증 요청마다** 필요하다(2FA 미등록 사용자를
// 막아야 하므로). 요청마다 SELECT를 한 번 더 하는 것은 이 앱의 다른 어떤 조회보다
// 잦은 쓰기가 되고, SQLite에서는 잠금 경합으로 이어진다. 값을 바꾸는 경로가
// SetSetting 하나뿐이므로 그곳에서 캐시를 버리면 오래된 값이 남을 수 없다.
func (s *Store) SecurityPolicy(ctx context.Context) (*SecurityPolicy, error) {
	s.policyMu.RLock()
	cached := s.policyCache
	s.policyMu.RUnlock()
	if cached != nil {
		copied := *cached
		return &copied, nil
	}

	p := &SecurityPolicy{}
	row := s.db.QueryRowContext(ctx,
		`SELECT value, updated_at, updated_by FROM app_settings WHERE key = ?`, SettingTOTPRequired)
	var value string
	var updatedAt sql.NullString
	var updatedBy sql.NullString
	err := row.Scan(&value, &updatedAt, &updatedBy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 설정한 적이 없으면 기본값이다: 각 사용자가 자율적으로 켠다.
	case err != nil:
		return nil, fmt.Errorf("read security policy: %w", err)
	default:
		p.TOTPRequired = value == "true"
		p.UpdatedAt = parseTimePtr(updatedAt)
		p.UpdatedBy = updatedBy.String
	}

	s.policyMu.Lock()
	s.policyCache = p
	s.policyMu.Unlock()

	copied := *p
	return &copied, nil
}

// SetTOTPRequired는 2단계 인증 의무화 여부를 바꾼다.
func (s *Store) SetTOTPRequired(ctx context.Context, required bool, actorID string) error {
	return s.SetSetting(ctx, SettingTOTPRequired, strconv.FormatBool(required), actorID)
}

func (s *Store) invalidatePolicy() {
	s.policyMu.Lock()
	s.policyCache = nil
	s.policyMu.Unlock()
}

// ClockOffset은 저장된 내부 시계 보정값을 읽는다. 기록이 없으면 0이다.
func (s *Store) ClockOffset(ctx context.Context) (time.Duration, error) {
	v, err := s.GetSetting(ctx, SettingClockOffset)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		// 값이 깨졌으면 0에서 다시 배우는 편이 낫다. 이상한 값으로 시계를 끌고 가면
		// 모든 사람의 로그인이 실패하고, 원인은 이 한 줄에 있다.
		return 0, nil
	}
	return time.Duration(secs) * time.Second, nil
}

// SaveClockOffset은 학습된 보정값을 남긴다. 재시작 후 처음부터 배우지 않기 위해서다.
func (s *Store) SaveClockOffset(ctx context.Context, d time.Duration) error {
	secs := int64(d.Round(time.Second) / time.Second)
	return s.SetSetting(ctx, SettingClockOffset, strconv.FormatInt(secs, 10), "")
}
