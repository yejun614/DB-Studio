package storage

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// AWS 가 문서에 실어 둔 서명 예제로 확인한다.
//
// 손으로 쓴 서명을 "우리가 만든 값과 우리가 만든 기댓값"으로 견주면 아무것도
// 확인하지 못한다. 남이 만든 정답이 있어야 검사가 검사가 된다 — 두 벡터가 모두
// 맞으면 우연히 맞을 여지는 사실상 없다(64자리 16진수 둘이다).
func TestSignV4MatchesAWSExamples(t *testing.T) {
	creds := Creds{
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:    "us-east-1",
		Service:   "s3",
	}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name, url, want string
	}{
		{
			// AWS 문서: Example: GET Bucket Lifecycle
			name: "질의 문자열에 값 없는 열쇠",
			url:  "https://examplebucket.s3.amazonaws.com/?lifecycle",
			want: "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			// AWS 문서: Example: Get Bucket (List Objects)
			name: "질의 문자열 둘",
			url:  "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J",
			want: "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet, tc.url, nil)
		if err != nil {
			t.Fatalf("%s: 요청을 만들지 못했습니다: %v", tc.name, err)
		}
		if err := SignV4(req, creds, emptyPayloadHash, at); err != nil {
			t.Fatalf("%s: 서명 실패: %v", tc.name, err)
		}
		auth := req.Header.Get("Authorization")
		if !strings.Contains(auth, "Signature="+tc.want) {
			t.Errorf("%s: 서명이 AWS 예제와 다릅니다\n얻은 값: %s\n기댓값 Signature=%s",
				tc.name, auth, tc.want)
		}
		if !strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
			t.Errorf("%s: 서명 대상 헤더가 다릅니다: %s", tc.name, auth)
		}
	}
}

// 임시 자격증명의 토큰은 헤더로 보내는 것으로 끝나지 않고 **서명 대상에도** 들어가야
// 한다. 빠뜨리면 STS 로 받은 키에서만 403 이 나는데, 그 차이는 오류 메시지에
// 드러나지 않는다.
func TestSessionTokenIsSigned(t *testing.T) {
	creds := Creds{
		AccessKey: "AKIA", SecretKey: "secret", Region: "us-east-1", Service: "s3",
		SessionToken: "FQoGZXIvYXdzEJr//////////wEaDA==",
	}
	req, _ := http.NewRequest(http.MethodGet, "https://b.s3.amazonaws.com/", nil)
	if err := SignV4(req, creds, emptyPayloadHash, time.Now()); err != nil {
		t.Fatalf("서명 실패: %v", err)
	}
	if req.Header.Get("X-Amz-Security-Token") == "" {
		t.Error("보안 토큰 헤더가 없습니다")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Errorf("보안 토큰이 서명 대상에 없습니다: %s", req.Header.Get("Authorization"))
	}
}

// 키 이름에는 공백·물결표·한글이 들어간다. url.QueryEscape 를 쓰면 공백이 '+' 가
// 되고 '~' 가 인코딩되는데, 둘 다 규약과 어긋나서 **그런 이름을 가진 객체에서만**
// 403 이 난다. 대부분의 버킷에서는 멀쩡하다가 어느 날 하나가 실패한다.
func TestURIEncodeFollowsRules(t *testing.T) {
	cases := map[string]string{
		"a b":     "a%20b",
		"a~b":     "a~b",
		"a+b":     "a%2Bb",
		"logs/x":  "logs/x",
		"한":       "%ED%95%9C",
		"a.b-c_d": "a.b-c_d",
	}
	for in, want := range cases {
		if got := uriEncode(in, false); got != want {
			t.Errorf("uriEncode(%q) = %q, 기댓값 %q", in, got, want)
		}
	}
	if got := uriEncode("logs/x", true); got != "logs%2Fx" {
		t.Errorf("질의 값의 슬래시는 인코딩해야 합니다: %q", got)
	}
}

// 키 이름 안의 슬래시는 경로 구분자다. 조각을 통째로 인코딩하면 다른 객체를 가리킨다.
func TestCanonicalURIKeepsSlashes(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet,
		"https://h/bucket/logs/2026-09/app log.txt", nil)
	got := canonicalURI(req.URL)
	want := "/bucket/logs/2026-09/app%20log.txt"
	if got != want {
		t.Errorf("canonicalURI = %q, 기댓값 %q", got, want)
	}
}
