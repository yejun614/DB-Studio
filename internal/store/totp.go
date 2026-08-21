package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// 2단계 인증(TOTP) 저장소.
//
// 공유 비밀은 봉인해서 넣고 꺼낼 때 푼다. 이 계층 밖으로는 항상 평문 base32가
// 오간다 — 봉인은 저장의 문제이지 호출부가 알아야 할 일이 아니다.

// TOTPEnrollment는 한 사용자의 2단계 인증 상태다.
type TOTPEnrollment struct {
	UserID string
	// Secret은 복호화된 base32 공유 비밀이다. 절대 JSON으로 내보내지 않는다.
	Secret string
	Digits int
	Period time.Duration
	// Skew는 이 사용자에게만 적용되는 시각 보정값이다.
	Skew        time.Duration
	LastStep    int64
	CreatedAt   time.Time
	ConfirmedAt *time.Time
	LastUsedAt  *time.Time
	Failures    int
	LockedUntil *time.Time
}

// Confirmed는 등록을 마쳤는지 반환한다. 마치지 않은 등록은 없는 것으로 취급해야 한다.
func (e *TOTPEnrollment) Confirmed() bool { return e != nil && e.ConfirmedAt != nil }

// Locked는 실패 누적으로 잠겨 있는지 반환한다.
func (e *TOTPEnrollment) Locked(now time.Time) bool {
	return e != nil && e.LockedUntil != nil && now.Before(*e.LockedUntil)
}

const totpColumns = `user_id, secret, digits, period, skew_seconds, last_step,
	created_at, confirmed_at, last_used_at, failures, locked_until`

func (s *Store) scanTOTP(row interface{ Scan(...any) error }) (*TOTPEnrollment, error) {
	var e TOTPEnrollment
	var sealed, createdAt string
	var periodSecs, skewSecs int64
	var confirmedAt, lastUsedAt, lockedUntil sql.NullString
	if err := row.Scan(&e.UserID, &sealed, &e.Digits, &periodSecs, &skewSecs, &e.LastStep,
		&createdAt, &confirmedAt, &lastUsedAt, &e.Failures, &lockedUntil); err != nil {
		return nil, err
	}
	secret, err := s.secret.Open(sealed)
	if err != nil {
		// 마스터 키가 바뀌면 여기서 걸린다. 조용히 "등록 안 됨"으로 되돌리면
		// 2FA가 소리 없이 꺼지므로, 오류로 올려 사람이 알게 한다.
		return nil, fmt.Errorf("unseal totp secret: %w", err)
	}
	e.Secret = secret
	e.Period = time.Duration(periodSecs) * time.Second
	e.Skew = time.Duration(skewSecs) * time.Second
	e.CreatedAt = parseTime(createdAt)
	e.ConfirmedAt = parseTimePtr(confirmedAt)
	e.LastUsedAt = parseTimePtr(lastUsedAt)
	e.LockedUntil = parseTimePtr(lockedUntil)
	return &e, nil
}

// GetTOTP은 등록 상태를 읽는다. 없으면 ErrNotFound다.
func (s *Store) GetTOTP(ctx context.Context, userID string) (*TOTPEnrollment, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+totpColumns+` FROM user_totp WHERE user_id = ?`, userID)
	e, err := s.scanTOTP(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get totp: %w", err)
	}
	return e, nil
}

// StartTOTPParams는 등록 시작 입력이다.
type StartTOTPParams struct {
	UserID string
	Secret string
	Digits int
	Period time.Duration
	// Skew는 등록 시작 시점의 추정 보정값이다. 확인 단계에서 실제 값으로 덮인다.
	Skew time.Duration
}

// StartTOTP은 아직 확인되지 않은 등록을 만든다(이미 있으면 덮어쓴다).
//
// 덮어쓰는 것이 맞다: 등록 화면을 다시 열었다는 것은 앞의 QR을 쓰지 않겠다는 뜻이다.
// 다만 **확인된 등록은 건드리지 않는다** — 이미 2FA를 켠 사람의 비밀을 새 QR로
// 갈아치우면, 화면을 열어 본 것만으로 기존 인증 앱이 무력해진다.
func (s *Store) StartTOTP(ctx context.Context, p StartTOTPParams) error {
	sealed, err := s.secret.Seal(p.Secret)
	if err != nil {
		return fmt.Errorf("seal totp secret: %w", err)
	}
	if p.Digits <= 0 {
		p.Digits = 6
	}
	if p.Period <= 0 {
		p.Period = 30 * time.Second
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO user_totp
		(user_id, secret, digits, period, skew_seconds, last_step, created_at)
		VALUES (?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT (user_id) DO UPDATE SET
			secret = excluded.secret, digits = excluded.digits, period = excluded.period,
			skew_seconds = excluded.skew_seconds, last_step = 0, created_at = excluded.created_at,
			confirmed_at = NULL, last_used_at = NULL, failures = 0, locked_until = NULL
		WHERE user_totp.confirmed_at IS NULL`,
		p.UserID, sealed, p.Digits, int64(p.Period/time.Second),
		int64(p.Skew/time.Second), nowString())
	if err != nil {
		return fmt.Errorf("start totp: %w", err)
	}
	// WHERE 절이 막았다는 뜻이다: 이미 확인된 등록이 있다. 조용히 넘어가면
	// 화면은 새 QR을 그려 주고 사용자는 그것을 등록하지만, 서버는 옛 비밀을
	// 계속 검사한다 — 원인을 짐작할 수 없는 실패가 된다.
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTOTPConfirmed
	}
	return nil
}

// ErrTOTPConfirmed는 이미 2단계 인증을 마친 계정에 새 등록을 시작하려 한 경우다.
var ErrTOTPConfirmed = errors.New("2단계 인증이 이미 설정되어 있습니다")

// ConfirmTOTP은 등록을 확정한다. 이 시점부터 로그인에 코드가 필요하다.
func (s *Store) ConfirmTOTP(ctx context.Context, userID string, skew time.Duration, step int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE user_totp
		SET confirmed_at = ?, last_used_at = ?, skew_seconds = ?, last_step = ?,
			failures = 0, locked_until = NULL
		WHERE user_id = ? AND confirmed_at IS NULL`,
		nowString(), nowString(), int64(skew/time.Second), step, userID)
	if err != nil {
		return fmt.Errorf("confirm totp: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordTOTPSuccess는 성공을 기록한다. 갱신된 보정값과 사용한 스텝을 함께 남겨
// 다음 검증이 같은 코드를 받아들이지 않게 한다.
func (s *Store) RecordTOTPSuccess(ctx context.Context, userID string, skew time.Duration, step int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_totp
		SET last_used_at = ?, skew_seconds = ?, last_step = ?, failures = 0, locked_until = NULL
		WHERE user_id = ?`,
		nowString(), int64(skew/time.Second), step, userID)
	if err != nil {
		return fmt.Errorf("record totp success: %w", err)
	}
	return nil
}

// RecordTOTPResync는 시각 보정값만 갱신한다. **인증 성공이 아니다.**
//
// 좁은 창(±1스텝)에서는 틀렸지만 넓게 훑으니 맞는 코드였다면, 그것은 코드가 아니라
// 시계가 문제라는 뜻이다. 그렇다고 그 코드로 로그인시켜 주면 인증에 쓰이는 창이
// 몇십 분으로 넓어지고, 어깨너머로 본 코드가 그동안 살아 있게 된다. 그래서 보정값만
// 고쳐 두고 다음 코드를 요구한다 — 다음 코드는 좁은 창에서 통과한다.
//
// last_step을 함께 올리는 것이 중요하다. 그러지 않으면 방금 넣은(혹은 가로챈) 그
// 코드가 보정된 창 안에서 곧바로 다시 유효해진다.
// failures는 건드리지 않는다. 시도 횟수 제한은 호출부가 따로 센다.
func (s *Store) RecordTOTPResync(ctx context.Context, userID string, skew time.Duration, step int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE user_totp SET skew_seconds = ?, last_step = ? WHERE user_id = ?`,
		int64(skew/time.Second), step, userID)
	if err != nil {
		return fmt.Errorf("record totp resync: %w", err)
	}
	return nil
}

// RecordTOTPFailure는 실패를 세고, 한도를 넘으면 잠근다. 잠금 해제 시각을 돌려준다.
func (s *Store) RecordTOTPFailure(ctx context.Context, userID string, limit int, lockFor time.Duration) (*time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var failures int
	err = tx.QueryRowContext(ctx, `SELECT failures FROM user_totp WHERE user_id = ?`, userID).Scan(&failures)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read totp failures: %w", err)
	}

	failures++
	var lockedUntil *time.Time
	var lockedArg any
	if failures >= limit {
		t := time.Now().UTC().Add(lockFor)
		lockedUntil = &t
		lockedArg = formatTime(t)
		failures = 0 // 잠근 뒤에는 카운터를 되돌린다. 잠금 자체가 기록이다.
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE user_totp SET failures = ?, locked_until = ? WHERE user_id = ?`,
		failures, lockedArg, userID); err != nil {
		return nil, fmt.Errorf("record totp failure: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return lockedUntil, nil
}

// DeleteTOTP은 등록과 복구 코드를 함께 지운다.
//
// 둘을 나눠 지울 수 있게 두지 않는 이유: 복구 코드만 남으면 그것은 비밀번호 없이
// 로그인할 수 있는 열쇠가 주인 없이 떠도는 것과 같다.
func (s *Store) DeleteTOTP(ctx context.Context, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM user_totp_recovery WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete recovery codes: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM user_totp WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("delete totp: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- 복구 코드 ----------

// ReplaceRecoveryCodes는 기존 코드를 전부 버리고 새 코드를 넣는다.
// 저장하는 것은 해시뿐이며, 원문은 호출부가 사용자에게 한 번 보여준 뒤 버린다.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID string, codes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM user_totp_recovery WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("clear recovery codes: %w", err)
	}
	now := nowString()
	for _, code := range codes {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_totp_recovery (id, user_id, code_hash, created_at) VALUES (?, ?, ?, ?)`,
			uuid.NewString(), userID, HashToken(code), now); err != nil {
			return fmt.Errorf("insert recovery code: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// UseRecoveryCode는 코드를 한 번만 쓰이게 소비한다. 쓸 수 있는 코드가 아니면 false다.
//
// UPDATE 한 문장으로 확인과 소비를 함께 하는 것이 핵심이다. 조회 후 갱신으로 나누면
// 같은 코드를 동시에 두 번 보내 두 세션을 얻을 수 있다.
func (s *Store) UseRecoveryCode(ctx context.Context, userID, code, ip string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE user_totp_recovery SET used_at = ?, used_ip = ?
			WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
		nowString(), ip, userID, HashToken(code))
	if err != nil {
		return false, fmt.Errorf("use recovery code: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountRecoveryCodes는 남은 개수와 전체 개수를 반환한다.
func (s *Store) CountRecoveryCodes(ctx context.Context, userID string) (remaining, total int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE used_at IS NULL), count(*)
			FROM user_totp_recovery WHERE user_id = ?`, userID).Scan(&remaining, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("count recovery codes: %w", err)
	}
	return remaining, total, nil
}

// ---------- 로그인 챌린지 ----------

// TOTPChallenge는 1단계(비밀번호)를 통과하고 2단계를 기다리는 상태다.
type TOTPChallenge struct {
	ID        string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
	Attempts  int
	IP        string
}

// CreateTOTPChallenge는 챌린지를 만든다. token은 원문이며 해시만 저장한다.
func (s *Store) CreateTOTPChallenge(ctx context.Context, token, userID, ip, ua string, ttl time.Duration) (*TOTPChallenge, error) {
	now := time.Now().UTC()
	ch := &TOTPChallenge{
		ID:        HashToken(token),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		IP:        ip,
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO totp_challenges
		(id, user_id, created_at, expires_at, attempts, ip, user_agent)
		VALUES (?, ?, ?, ?, 0, ?, ?)`,
		ch.ID, userID, formatTime(ch.CreatedAt), formatTime(ch.ExpiresAt), ip, truncate(ua, 512)); err != nil {
		return nil, fmt.Errorf("insert totp challenge: %w", err)
	}
	return ch, nil
}

// LookupTOTPChallenge는 토큰으로 챌린지를 찾는다. 만료된 것은 지우고 없는 것으로 취급한다.
func (s *Store) LookupTOTPChallenge(ctx context.Context, token string) (*TOTPChallenge, error) {
	id := HashToken(token)
	var ch TOTPChallenge
	var created, expires string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, created_at, expires_at, attempts, ip FROM totp_challenges WHERE id = ?`, id).
		Scan(&ch.ID, &ch.UserID, &created, &expires, &ch.Attempts, &ch.IP)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup totp challenge: %w", err)
	}
	ch.CreatedAt = parseTime(created)
	ch.ExpiresAt = parseTime(expires)
	if time.Now().UTC().After(ch.ExpiresAt) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM totp_challenges WHERE id = ?`, id)
		return nil, ErrNotFound
	}
	return &ch, nil
}

// BumpTOTPChallengeAttempt는 시도 횟수를 올리고 누적값을 돌려준다.
func (s *Store) BumpTOTPChallengeAttempt(ctx context.Context, token string) (int, error) {
	id := HashToken(token)
	var attempts int
	err := s.db.QueryRowContext(ctx,
		`UPDATE totp_challenges SET attempts = attempts + 1 WHERE id = ? RETURNING attempts`, id).
		Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("bump totp challenge: %w", err)
	}
	return attempts, nil
}

func (s *Store) DeleteTOTPChallenge(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM totp_challenges WHERE id = ?`, HashToken(token))
	if err != nil {
		return fmt.Errorf("delete totp challenge: %w", err)
	}
	return nil
}

// PurgeExpiredTOTPChallenges는 만료된 챌린지를 정리한다.
// 세션 정리와 같은 주기 작업에서 부른다.
func (s *Store) PurgeExpiredTOTPChallenges(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM totp_challenges WHERE expires_at < ?`, nowString())
	if err != nil {
		return 0, fmt.Errorf("purge totp challenges: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
