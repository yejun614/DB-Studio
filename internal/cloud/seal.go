package cloud

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 백업 파일 암호화 스트림.
//
// 메타 DB에는 비밀번호 해시·감사 기록·설계 문서가 평문으로 있다(자격증명만 봉인되어 있다).
// 그 파일을 남의 저장소에 올리면 버킷 권한 하나가 그 전부를 여는 열쇠가 된다. 그래서
// 마스터 키에서 파생한 키로 암호화해 올린다 — 복구에는 어차피 같은 마스터 키가 필요하므로
// (봉인된 자격증명을 풀어야 한다) 지켜야 할 열쇠가 늘지 않는다.
//
// 형식: 머리 8바이트("DBSENC1\x00") + 무작위 접두 8바이트, 이어서 조각들.
// 조각은 [4바이트 길이][AES-256-GCM 암호문]이고, nonce는 접두 8바이트 + 조각 번호 4바이트다.
// 마지막 조각은 추가 인증 데이터(AAD)가 1이다. 그래서 끝을 잘라 낸 파일은 "마지막 조각이
// 없다"로 드러난다 — 조각 단위 GCM만으로는 잘린 백업이 멀쩡한 백업처럼 읽힌다.

var sealMagic = []byte("DBSENC1\x00")

const sealChunk = 64 << 10

// sealWriter는 쓰는 대로 암호화해 아래 Writer로 내보낸다.
type sealWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	prefix [8]byte
	n      uint32
	buf    []byte
	closed bool
}

// NewSealWriter는 암호화 스트림을 연다. Close를 불러야 마지막 조각이 나간다.
func NewSealWriter(w io.Writer, key []byte) (io.WriteCloser, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	s := &sealWriter{w: w, aead: aead, buf: make([]byte, 0, sealChunk)}
	if _, err := rand.Read(s.prefix[:]); err != nil {
		return nil, err
	}
	if _, err := w.Write(sealMagic); err != nil {
		return nil, err
	}
	if _, err := w.Write(s.prefix[:]); err != nil {
		return nil, err
	}
	return s, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *sealWriter) nonce() []byte {
	n := make([]byte, 12)
	copy(n, s.prefix[:])
	binary.BigEndian.PutUint32(n[8:], s.n)
	return n
}

func (s *sealWriter) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("암호화 스트림이 이미 닫혔습니다")
	}
	written := 0
	for len(p) > 0 {
		room := sealChunk - len(s.buf)
		take := min(room, len(p))
		s.buf = append(s.buf, p[:take]...)
		p = p[take:]
		written += take
		if len(s.buf) == sealChunk {
			if err := s.emit(false); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (s *sealWriter) emit(final bool) error {
	aad := []byte{0}
	if final {
		aad[0] = 1
	}
	ct := s.aead.Seal(nil, s.nonce(), s.buf, aad)
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], uint32(len(ct)))
	if _, err := s.w.Write(head[:]); err != nil {
		return err
	}
	if _, err := s.w.Write(ct); err != nil {
		return err
	}
	s.n++
	s.buf = s.buf[:0]
	return nil
}

func (s *sealWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.emit(true)
}

// openReader는 암호화 스트림을 푼다.
type openReader struct {
	r      io.Reader
	aead   cipher.AEAD
	prefix [8]byte
	n      uint32
	buf    *bytes.Reader
	done   bool
}

// NewOpenReader는 NewSealWriter가 만든 스트림을 푸는 Reader를 만든다.
func NewOpenReader(r io.Reader, key []byte) (io.Reader, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	head := make([]byte, len(sealMagic)+8)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, fmt.Errorf("암호화된 백업의 머리를 읽지 못했습니다: %w", err)
	}
	if !bytes.Equal(head[:len(sealMagic)], sealMagic) {
		return nil, errors.New("DB Studio 가 암호화한 백업이 아닙니다")
	}
	o := &openReader{r: r, aead: aead, buf: bytes.NewReader(nil)}
	copy(o.prefix[:], head[len(sealMagic):])
	return o, nil
}

func (o *openReader) Read(p []byte) (int, error) {
	for o.buf.Len() == 0 {
		if o.done {
			return 0, io.EOF
		}
		if err := o.next(); err != nil {
			return 0, err
		}
	}
	return o.buf.Read(p)
}

func (o *openReader) next() error {
	var head [4]byte
	if _, err := io.ReadFull(o.r, head[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errors.New("백업 파일이 중간에 끊겨 있습니다(마지막 조각이 없습니다)")
		}
		return err
	}
	size := binary.BigEndian.Uint32(head[:])
	if size > sealChunk+64 {
		return errors.New("백업 파일의 조각 크기가 올바르지 않습니다")
	}
	ct := make([]byte, size)
	if _, err := io.ReadFull(o.r, ct); err != nil {
		return errors.New("백업 파일이 중간에 끊겨 있습니다")
	}
	nonce := make([]byte, 12)
	copy(nonce, o.prefix[:])
	binary.BigEndian.PutUint32(nonce[8:], o.n)
	o.n++
	// 마지막 조각인지 모르므로 둘 다 시도한다. AAD가 틀리면 인증이 실패할 뿐이다.
	if pt, err := o.aead.Open(nil, nonce, ct, []byte{0}); err == nil {
		o.buf = bytes.NewReader(pt)
		return nil
	}
	pt, err := o.aead.Open(nil, nonce, ct, []byte{1})
	if err != nil {
		return errors.New("백업을 풀지 못했습니다 — 다른 마스터 키로 암호화됐거나 파일이 손상됐습니다")
	}
	o.buf = bytes.NewReader(pt)
	o.done = true
	return nil
}
