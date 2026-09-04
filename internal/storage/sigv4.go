package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4 서명.
//
// SDK 를 쓰지 않고 손으로 쓴 이유는 하둡·Ceph 와 같다: 이 앱은 CGO 없는 단일
// 바이너리이고, 의존성 하나가 늘 때마다 그 바이너리가 무거워진다. S3 에서 우리가
// 쓰는 것은 목록 세 가지와 HEAD 하나뿐이라, SDK 가 주는 것의 대부분(재시도 정책,
// 자격증명 체인, 멀티파트 업로드, 페이지네이터)은 쓰지 않는다.
//
// 서명은 규약이 짧고 **틀리면 곧바로 403 이 난다**. 조용히 잘못될 여지가 없다는
// 것이 손으로 쓸 만한 근거다 — 조용히 어긋나는 코드였다면 SDK 를 골랐을 것이다.

const (
	sigAlgorithm = "AWS4-HMAC-SHA256"
	// emptyPayloadHash는 본문 없는 요청(GET·HEAD)의 SHA-256 이다.
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Creds는 서명에 쓰는 자격증명이다.
type Creds struct {
	AccessKey string
	SecretKey string
	// SessionToken은 임시 자격증명(STS)에만 있다. 있으면 헤더로 함께 보내고
	// 서명 대상에도 넣는다 — 넣지 않으면 서버가 계산한 서명과 달라진다.
	SessionToken string
	Region       string
	Service      string
}

// SignV4는 요청에 Authorization 헤더를 붙인다.
//
// payloadHash는 본문의 SHA-256 16진수다. 본문이 없으면 emptyPayloadHash 를 넘긴다.
// S3 는 이 값을 x-amz-content-sha256 헤더로도 요구하므로 함께 넣는다.
func SignV4(req *http.Request, c Creds, payloadHash string, now time.Time) error {
	if strings.TrimSpace(c.AccessKey) == "" || strings.TrimSpace(c.SecretKey) == "" {
		return fmt.Errorf("액세스 키와 시크릿 키가 필요합니다")
	}
	if payloadHash == "" {
		payloadHash = emptyPayloadHash
	}
	region := c.Region
	if region == "" {
		// 리전을 모르는 서버(MinIO 등)도 서명 자체는 리전 문자열을 요구한다.
		// us-east-1 은 그런 서버들이 관례로 받아 주는 값이다.
		region = "us-east-1"
	}
	service := c.Service
	if service == "" {
		service = "s3"
	}

	stamp := now.UTC().Format("20060102T150405Z")
	day := now.UTC().Format("20060102")

	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	if req.Host != "" {
		req.Header.Set("Host", req.Host)
	} else {
		req.Header.Set("Host", req.URL.Host)
	}

	signed, canonicalHeaders := canonicalHeaderBlock(req)
	canonical := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signed,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{day, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		sigAlgorithm, stamp, scope, hashHex([]byte(canonical)),
	}, "\n")

	key := signingKey(c.SecretKey, day, region, service)
	signature := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm, c.AccessKey, scope, signed, signature))
	return nil
}

func signingKey(secret, day, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(day))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalHeaderBlock은 서명할 헤더 목록과 그 본문을 만든다.
//
// host 와 x-amz-* 만 서명한다. 서명 대상이 많을수록 중간 프록시가 헤더 하나를
// 손대는 순간 서명이 깨지는데, S3 는 이 셋만으로 충분하다.
func canonicalHeaderBlock(req *http.Request) (signed, block string) {
	names := []string{"host"}
	for name := range req.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") {
			names = append(names, lower)
		}
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		value := req.Header.Get(name)
		if name == "host" {
			value = req.Header.Get("Host")
			if value == "" {
				value = req.URL.Host
			}
		}
		fmt.Fprintf(&b, "%s:%s\n", name, strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(names, ";"), b.String()
}

// canonicalURI는 경로를 규약대로 인코딩한다.
//
// S3 는 경로를 **한 번만** 인코딩한다(다른 서비스는 두 번 한다). 슬래시는 그대로
// 두고 각 조각만 인코딩한다 — 조각을 통째로 인코딩하면 키 이름 안의 슬래시가
// 경로 구분자가 아니게 되어 다른 객체를 가리킨다.
func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		// EscapedPath 가 이미 인코딩한 것을 되돌린 뒤 규약대로 다시 인코딩한다.
		raw, err := url.PathUnescape(p)
		if err != nil {
			raw = p
		}
		parts[i] = uriEncode(raw, false)
	}
	return strings.Join(parts, "/")
}

// canonicalQuery는 질의 문자열을 이름순으로 정렬해 인코딩한다.
func canonicalQuery(u *url.URL) string {
	values := u.Query()
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := []string{}
	for _, k := range keys {
		vs := append([]string{}, values[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			pairs = append(pairs, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(pairs, "&")
}

// uriEncode는 AWS 규약의 인코딩이다.
//
// url.QueryEscape 를 쓰지 않는 이유: 그것은 공백을 '+' 로 바꾸고 '~' 를 인코딩한다.
// 둘 다 규약과 어긋나서, 이름에 공백이나 물결표가 들어간 키에서만 403 이 난다 —
// 대부분의 버킷에서는 멀쩡하다가 어느 날 한 객체에서 실패하는 종류의 어긋남이다.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'),
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/':
			if encodeSlash {
				b.WriteString("%2F")
			} else {
				b.WriteByte(c)
			}
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
