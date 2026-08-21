package totp

import (
	"testing"
	"time"
)

// RFC 4226 Appendix D의 HOTP 테스트 벡터.
// 공유 비밀은 ASCII "12345678901234567890"이다.
func TestHOTPRFC4226(t *testing.T) {
	key := []byte("12345678901234567890")
	want := []string{
		"755224", "287082", "359152", "969429", "338314",
		"254676", "287922", "162583", "399871", "520489",
	}
	for step, code := range want {
		if got := codeForStep(key, int64(step), 6); got != code {
			t.Errorf("step %d: got %s, want %s", step, got, code)
		}
	}
}

// RFC 6238 Appendix B의 TOTP 테스트 벡터(SHA-1 부분).
// 8자리로 주어져 있으므로 Digits=8로 확인한다.
func TestTOTPRFC6238(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	p := Params{Digits: 8, Period: 30 * time.Second}

	cases := []struct {
		unix int64
		code string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, tc := range cases {
		got, err := CodeAt(secret, time.Unix(tc.unix, 0).UTC(), p)
		if err != nil {
			t.Fatalf("unix %d: %v", tc.unix, err)
		}
		if got != tc.code {
			t.Errorf("unix %d: got %s, want %s", tc.unix, got, tc.code)
		}
	}
}

func TestGenerateSecretRoundTrip(t *testing.T) {
	s, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 32 { // 20바이트 → base32 32글자(패딩 없음)
		t.Fatalf("secret length = %d, want 32: %q", len(s), s)
	}
	key, err := DecodeSecret(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != secretBytes {
		t.Fatalf("key length = %d, want %d", len(key), secretBytes)
	}
	// 사람이 옮겨 적은 형태(공백·소문자)도 같은 키로 풀려야 한다.
	spaced, err := DecodeSecret(FormatSecret(s))
	if err != nil {
		t.Fatalf("공백이 섞인 비밀을 거부했습니다: %v", err)
	}
	if string(spaced) != string(key) {
		t.Error("공백을 넣은 비밀이 다른 키로 풀렸습니다")
	}
}

// Verify는 참/거짓만이 아니라 **몇 칸 어긋났는지**를 돌려줘야 한다.
// 이 값이 없으면 시계 보정을 학습할 수 없다.
func TestVerifyReportsDelta(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultParams()
	now := time.Unix(1_700_000_000, 0).UTC()

	for _, want := range []int{-20, -3, -1, 0, 1, 5, 20} {
		at := now.Add(time.Duration(want) * p.Period)
		code, err := CodeAt(secret, at, p)
		if err != nil {
			t.Fatal(err)
		}
		delta, step, ok := Verify(secret, code, now, 20, p)
		if !ok {
			t.Fatalf("delta %d 코드를 찾지 못했습니다", want)
		}
		if delta != want {
			t.Errorf("delta = %d, want %d", delta, want)
		}
		if step != p.Step(at) {
			t.Errorf("step = %d, want %d", step, p.Step(at))
		}
	}
}

func TestVerifyWindowIsBounded(t *testing.T) {
	secret, _ := GenerateSecret()
	p := DefaultParams()
	now := time.Unix(1_700_000_000, 0).UTC()

	code, _ := CodeAt(secret, now.Add(5*p.Period), p)
	if _, _, ok := Verify(secret, code, now, 1, p); ok {
		t.Error("윈도우 밖의 코드를 받아들였습니다")
	}
	if _, _, ok := Verify(secret, "", now, 1, p); ok {
		t.Error("빈 코드를 받아들였습니다")
	}
	if _, _, ok := Verify(secret, "12345", now, 1, p); ok {
		t.Error("자리수가 다른 코드를 받아들였습니다")
	}
}

func TestVerifyAcceptsSpacedCode(t *testing.T) {
	secret, _ := GenerateSecret()
	p := DefaultParams()
	now := time.Unix(1_700_000_000, 0).UTC()
	code, _ := CodeAt(secret, now, p)
	spaced := code[:3] + " " + code[3:]
	if _, _, ok := Verify(secret, spaced, now, 1, p); !ok {
		t.Error("공백이 섞인 코드를 거부했습니다")
	}
}

func TestURI(t *testing.T) {
	got := URI("DB Studio", "alice", "ABCDEFGH", DefaultParams())
	want := "otpauth://totp/DB%20Studio:alice?" +
		"algorithm=SHA1&digits=6&issuer=DB+Studio&period=30&secret=ABCDEFGH"
	if got != want {
		t.Errorf("URI = %s\nwant %s", got, want)
	}
}
