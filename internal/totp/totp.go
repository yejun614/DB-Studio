// Package totp는 RFC 4226(HOTP)과 RFC 6238(TOTP)을 구현한다.
//
// 외부 의존성을 쓰지 않는 이유는 이 앱의 다른 부분과 같다: 이 바이너리는 인터넷이
// 없는 망에서도 단독으로 돌아야 하고, 인증 경로에 들어가는 코드는 우리가 읽을 수
// 있어야 한다. 알고리즘 자체는 30줄이면 끝난다 — 어려운 것은 시각을 맞추는 쪽이고,
// 그 문제는 internal/clock이 담당한다.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultDigits는 인증 앱들이 사실상 유일하게 쓰는 자리수다.
	DefaultDigits = 6
	// DefaultPeriod는 코드 하나가 유효한 시간이다.
	DefaultPeriod = 30 * time.Second
	// secretBytes는 생성하는 공유 비밀의 길이다. RFC 4226은 최소 128비트를 요구하고
	// 160비트를 권장한다(HMAC-SHA1의 출력 길이와 같다).
	secretBytes = 20
)

var (
	ErrBadSecret = errors.New("공유 비밀 형식이 올바르지 않습니다")
	ErrBadCode   = errors.New("인증 코드 형식이 올바르지 않습니다")
)

// b32는 표준 base32에서 패딩만 뺀 인코딩이다.
//
// 패딩('=')을 빼는 이유: otpauth URI의 secret 파라미터에 그대로 들어가고, 사람이
// 손으로 옮겨 적기도 한다. '='는 URL에서 이스케이프되고 손으로 적을 때 빠뜨리기
// 쉬워서 실패 원인이 되는데, 인증 앱들은 어차피 패딩 없는 문자열을 받아들인다.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret은 새 공유 비밀을 base32 문자열로 만든다.
func GenerateSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return b32.EncodeToString(buf), nil
}

// DecodeSecret은 base32 문자열을 키 바이트로 되돌린다.
//
// 공백과 소문자를 허용하는 이유: 사람이 옮겨 적은 값이 들어올 수 있다. 인증 앱들이
// 화면에 4글자씩 끊어 보여주므로 그대로 복사하면 공백이 섞인다.
func DecodeSecret(secret string) ([]byte, error) {
	cleaned := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(secret))
	cleaned = strings.TrimRight(cleaned, "=")
	if cleaned == "" {
		return nil, ErrBadSecret
	}
	key, err := b32.DecodeString(cleaned)
	if err != nil {
		return nil, ErrBadSecret
	}
	if len(key) < 10 {
		return nil, ErrBadSecret
	}
	return key, nil
}

// Params는 코드 생성 규칙이다. 제로값은 쓰지 말고 DefaultParams를 기준으로 바꾼다.
type Params struct {
	Digits int
	Period time.Duration
}

func DefaultParams() Params {
	return Params{Digits: DefaultDigits, Period: DefaultPeriod}
}

func (p Params) normalize() Params {
	if p.Digits < 6 || p.Digits > 8 {
		p.Digits = DefaultDigits
	}
	if p.Period <= 0 {
		p.Period = DefaultPeriod
	}
	return p
}

// Step은 시각을 카운터 값(스텝 번호)으로 바꾼다.
//
// 스텝 번호를 밖으로 노출하는 이유: 재사용 방지(같은 코드를 두 번 못 쓰게 하는 것)는
// "마지막으로 쓴 스텝"을 기록해야 하고, 그 값은 저장 계층까지 올라가야 한다.
func (p Params) Step(t time.Time) int64 {
	p = p.normalize()
	return t.Unix() / int64(p.Period/time.Second)
}

// CodeAt은 주어진 시각의 코드를 만든다.
func CodeAt(secret string, t time.Time, p Params) (string, error) {
	key, err := DecodeSecret(secret)
	if err != nil {
		return "", err
	}
	p = p.normalize()
	return codeForStep(key, p.Step(t), p.Digits), nil
}

// codeForStep은 RFC 4226의 HOTP다.
func codeForStep(key []byte, step int64, digits int) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))

	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)

	// 동적 절단(dynamic truncation): 마지막 바이트의 하위 4비트가 시작 위치를 가리킨다.
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	mod := uint32(math.Pow10(digits))
	return fmt.Sprintf("%0*d", digits, value%mod)
}

// NormalizeCode는 사용자가 입력한 코드에서 공백과 하이픈을 제거한다.
// 인증 앱이 "123 456"처럼 보여주므로 그대로 붙여넣는 사람이 많다.
func NormalizeCode(code string) string {
	return strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(strings.TrimSpace(code))
}

// Verify는 t를 중심으로 ±window 스텝을 훑어 코드를 찾는다.
//
// 반환하는 delta는 **맞은 스텝이 t로부터 몇 칸 떨어져 있는가**이며, 이 값이 이
// 패키지의 핵심 산출물이다. 단순히 참/거짓만 돌려주면 "우리 시계가 얼마나 틀렸는가"를
// 알 수 없고, 그러면 시계가 어긋난 서버는 영원히 어긋난 채로 남는다.
// 호출부(internal/auth)는 delta를 보정값 학습에 쓴다.
//
// step은 실제로 맞은 절대 스텝 번호다. 재사용 방지에 쓴다.
func Verify(secret, code string, t time.Time, window int, p Params) (delta int, step int64, ok bool) {
	key, err := DecodeSecret(secret)
	if err != nil {
		return 0, 0, false
	}
	p = p.normalize()
	code = NormalizeCode(code)
	if len(code) != p.Digits {
		return 0, 0, false
	}
	if window < 0 {
		window = 0
	}

	center := p.Step(t)
	// 가까운 쪽부터 본다. 시계가 거의 맞는 정상 상태에서 첫 시도에 끝나고,
	// 어긋난 서버에서는 가장 그럴듯한(가장 작은) 보정값을 고르게 된다.
	for d := 0; d <= window; d++ {
		for _, cand := range candidates(d) {
			s := center + int64(cand)
			// ConstantTimeCompare를 쓰는 이유는 문자열 비교의 조기 종료로
			// 앞자리가 맞았는지가 시간으로 새어 나가는 것을 막기 위해서다.
			if subtle.ConstantTimeCompare([]byte(codeForStep(key, s, p.Digits)), []byte(code)) == 1 {
				return cand, s, true
			}
		}
	}
	return 0, 0, false
}

// candidates는 거리 d에 있는 오프셋들을 돌려준다(0이면 하나, 그 외에는 앞뒤 둘).
func candidates(d int) []int {
	if d == 0 {
		return []int{0}
	}
	return []int{-d, d}
}

// URI는 인증 앱이 읽는 otpauth URI를 만든다.
//
// 라벨을 "발급자:계정" 형태로 두는 것은 관행이다. issuer 파라미터만 주고 라벨에서
// 빼면 일부 앱이 목록에서 계정만 보여줘, DB Studio가 여러 대일 때 어느 것인지
// 구분할 수 없게 된다.
func URI(issuer, account, secret string, p Params) string {
	p = p.normalize()
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(p.Digits))
	q.Set("period", fmt.Sprint(int(p.Period/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// FormatSecret은 손으로 옮겨 적을 수 있게 4글자씩 끊어 준다.
func FormatSecret(secret string) string {
	var b strings.Builder
	for i, r := range secret {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}
