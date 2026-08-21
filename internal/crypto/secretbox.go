package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
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
	return &SecretBox{aead: aead}, nil
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
