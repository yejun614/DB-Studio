package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SecretBox는 DB 접속 자격증명 같은 민감 문자열을 AES-256-GCM으로 봉인한다.
// 출력 형식은 "v1:<base64(nonce||ciphertext)>" 이며, 향후 키 로테이션을 위해 버전 접두사를 둔다.
type SecretBox struct {
	aead cipher.AEAD
	// key는 파생 키를 만들 때만 쓴다(DeriveKey·Fingerprint). 봉인 자체는 aead가 한다.
	key []byte
}

const secretVersion = "v1"

var (
	ErrKeySize       = errors.New("master key must be 32 bytes")
	ErrBadCiphertext = errors.New("invalid ciphertext")
)

// NewSecretBox는 32바이트 키로 봉인기를 만든다.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != 32 {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return &SecretBox{aead: aead, key: append([]byte(nil), key...)}, nil
}

// DeriveKey는 용도 이름(label)마다 다른 32바이트 키를 만든다.
//
// 마스터 키를 그대로 다른 용도(백업 파일 암호화 등)에 쓰지 않는 이유: 같은 키가 두 가지
// 형식의 암호문을 만들면, 한쪽 형식의 약점이 다른 쪽까지 번진다. 용도마다 키를 갈라 두면
// 한 용도의 사고가 그 용도에 머문다.
func (s *SecretBox) DeriveKey(label string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// Fingerprint는 마스터 키를 가리키는 짧은 표식이다. 키 자체는 드러나지 않는다.
//
// 원격(D1·오브젝트 스토리지)에 있는 데이터가 **이 키로 봉인된 것인지**를 받기 전에 확인하는
// 데 쓴다. 다른 키로 봉인된 메타 DB를 받으면 저장된 자격증명이 전부 풀리지 않는데, 그 사실은
// 받고 나서 커넥션을 하나씩 열어 볼 때에야 드러난다.
func (s *SecretBox) Fingerprint() string {
	return hex.EncodeToString(s.DeriveKey("dbstudio/key-fingerprint/v1"))[:16]
}

// Seal은 평문을 암호화한다. 빈 문자열은 빈 문자열로 통과시켜 "값 없음"을 유지한다.
func (s *SecretBox) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("read nonce: %w", err)
	}
	ct := s.aead.Seal(nil, nonce, []byte(plaintext), nil)
	return secretVersion + ":" + base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

// Open은 Seal의 역연산이다.
func (s *SecretBox) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	version, payload, ok := strings.Cut(sealed, ":")
	if !ok || version != secretVersion {
		return "", ErrBadCiphertext
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", ErrBadCiphertext
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns+1 {
		return "", ErrBadCiphertext
	}
	pt, err := s.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	return string(pt), nil
}

// LoadOrCreateMasterKey는 envValue(base64)가 있으면 그것을 쓰고,
// 없으면 keyPath의 키파일을 읽거나 새로 생성한다.
// 두 번째 반환값은 키파일을 새로 만들었는지 여부(경고 출력용)다.
func LoadOrCreateMasterKey(envValue, keyPath string) ([]byte, bool, error) {
	if envValue != "" {
		key, err := decodeKey(envValue)
		if err != nil {
			return nil, false, fmt.Errorf("DBSTUDIO_MASTER_KEY: %w", err)
		}
		return key, false, nil
	}

	data, err := os.ReadFile(keyPath)
	if err == nil {
		key, derr := decodeKey(strings.TrimSpace(string(data)))
		if derr != nil {
			return nil, false, fmt.Errorf("%s: %w", keyPath, derr)
		}
		return key, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("read key file: %w", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, false, fmt.Errorf("generate key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, false, fmt.Errorf("create key dir: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key) + "\n"
	if err := os.WriteFile(keyPath, []byte(encoded), 0o600); err != nil {
		return nil, false, fmt.Errorf("write key file: %w", err)
	}
	return key, true, nil
}

func decodeKey(s string) ([]byte, error) {
	for _, dec := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := dec.DecodeString(s); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	return nil, ErrKeySize
}
