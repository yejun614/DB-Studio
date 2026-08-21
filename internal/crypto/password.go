// Package crypto는 비밀번호 해싱과 저장 시크릿 암호화를 담당한다.
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id 파라미터. 변경 시 기존 해시는 인코딩된 값으로 계속 검증된다.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

var ErrBadHashFormat = errors.New("invalid password hash format")

// HashPassword는 argon2id 해시를 PHC 문자열 형식으로 반환한다.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword는 평문과 저장된 해시를 상수시간 비교한다.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrBadHashFormat
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrBadHashFormat
	}
	var memory uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, ErrBadHashFormat
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHashFormat
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrBadHashFormat
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// base58 알파벳: 0/O/I/l 처럼 혼동되는 문자를 제외해 터미널 출력 후 수동 입력에 적합하다.
const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// GeneratePassword는 사람이 옮겨 적을 수 있는 랜덤 비밀번호를 만든다.
func GeneratePassword(length int) (string, error) {
	if length <= 0 {
		length = 24
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	// rand.Read로 채운 바이트를 알파벳 길이로 매핑한다. 58은 256을 나누지 못하므로
	// 편향을 없애기 위해 허용 범위(232 = 58*4) 밖의 값은 재추첨한다.
	const limit = 232
	out := make([]byte, 0, length)
	for len(out) < length {
		for _, b := range buf {
			if b >= limit {
				continue
			}
			out = append(out, base58[int(b)%len(base58)])
			if len(out) == length {
				break
			}
		}
		if len(out) < length {
			if _, err := rand.Read(buf); err != nil {
				return "", fmt.Errorf("read random: %w", err)
			}
		}
	}
	return string(out), nil
}

// recoveryAlphabet은 복구 코드용 문자 집합이다.
//
// 소문자와 숫자만 쓰고 헷갈리는 글자(i·l·1, o·0)를 뺀다. 복구 코드는 종이에 적어
// 서랍에 넣어 두었다가 몇 달 뒤 옮겨 적는 물건이다 — 그때 대소문자를 구분해야 하거나
// 0과 O를 구별해야 하면, 코드가 맞는데도 틀렸다고 나오는 상황이 된다.
const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// GenerateRecoveryCode는 2단계 인증 복구 코드 하나를 만든다.
// 형식은 "xxxx-xxxx-xxxx-xxxx"이며 하이픈은 읽기 위한 것이다(검증 때 무시한다).
func GenerateRecoveryCode() (string, error) {
	const groups, perGroup = 4, 4
	letters, err := randomLetters(recoveryAlphabet, groups*perGroup)
	if err != nil {
		return "", err
	}
	parts := make([]string, groups)
	for i := range parts {
		parts[i] = letters[i*perGroup : (i+1)*perGroup]
	}
	return strings.Join(parts, "-"), nil
}

// randomLetters는 알파벳에서 편향 없이 n글자를 뽑는다.
// 알파벳 길이가 256을 나누지 못하므로 허용 범위 밖의 바이트는 버린다.
func randomLetters(alphabet string, n int) (string, error) {
	limit := 256 - (256 % len(alphabet))
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("read random: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// RandomToken은 세션 토큰 등에 쓰는 URL-safe 랜덤 문자열을 만든다.
func RandomToken(nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// PasswordPolicyError는 비밀번호 정책 위반을 나타낸다.
type PasswordPolicyError struct{ Reason string }

func (e *PasswordPolicyError) Error() string { return e.Reason }

// CheckPasswordPolicy는 최소 요건을 검증한다.
func CheckPasswordPolicy(pw string) error {
	if len([]rune(pw)) < 10 {
		return &PasswordPolicyError{Reason: "비밀번호는 10자 이상이어야 합니다"}
	}
	if len(pw) > 256 {
		return &PasswordPolicyError{Reason: "비밀번호가 너무 깁니다 (최대 256바이트)"}
	}
	var hasLetter, hasOther bool
	for _, r := range pw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		default:
			hasOther = true
		}
	}
	if !hasLetter || !hasOther {
		return &PasswordPolicyError{Reason: "비밀번호는 영문자와 숫자/기호를 함께 포함해야 합니다"}
	}
	return nil
}
