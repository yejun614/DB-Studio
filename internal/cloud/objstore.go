package cloud

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/storage"
	"dbstudio/internal/store"
)

// S3 규약 오브젝트 스토리지에 올리고 내려받는 클라이언트.
//
// 스토리지 화면의 클라이언트(storage.S3)와 따로 둔 이유: 그쪽은 **읽기 전용**이라는
// 약속을 하고 있다(지우는 판단을 버튼 하나로 옮기지 않는다). 이쪽은 백업을 올리고,
// 보존 개수를 넘은 **자기 백업**을 지운다. 같은 타입에 쓰기를 더하면 그 약속이 흐려진다.
// 서명(SigV4)은 같은 것을 쓴다 — 틀리면 곧바로 403이 나는 코드라 한 벌이면 충분하다.

// ObjectStore는 버킷 하나에 대한 클라이언트다.
type ObjectStore struct {
	scheme    string
	host      string
	bucket    string
	prefix    string
	virtual   bool
	creds     storage.Creds
	http      *http.Client
	signClock func() time.Time
}

// NewObjectStore는 설정에서 클라이언트를 만든다.
func NewObjectStore(cfg store.ObjectSettings) (*ObjectStore, error) {
	endpoint, region, err := objectEndpoint(cfg)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("엔드포인트 주소가 올바르지 않습니다: %s", endpoint)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("엔드포인트는 https:// 또는 http:// 로 시작해야 합니다")
	}
	prefix := strings.TrimLeft(strings.TrimSpace(cfg.Prefix), "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &ObjectStore{
		scheme: u.Scheme, host: u.Host,
		bucket: strings.TrimSpace(cfg.Bucket), prefix: prefix,
		// R2는 가상 호스트도 받지만 경로 방식이 어디서나 된다(storage.S3.pathStyle 참고).
		virtual: cfg.VirtualHost && cfg.Provider != "r2",
		creds: storage.Creds{
			AccessKey: strings.TrimSpace(cfg.AccessKey),
			SecretKey: strings.TrimSpace(cfg.SecretKey),
			Region:    region, Service: "s3",
		},
		// 업로드는 크기에 비례해 걸린다. 전체 시간에 상한을 두지 않고 ctx로 끊는다.
		http:      &http.Client{},
		signClock: time.Now,
	}, nil
}

// objectEndpoint는 공급자별로 엔드포인트와 서명 리전을 정한다.
func objectEndpoint(cfg store.ObjectSettings) (string, string, error) {
	region := strings.TrimSpace(cfg.Region)
	switch cfg.Provider {
	case "r2":
		acct := strings.TrimSpace(cfg.AccountID)
		if acct == "" {
			return "", "", fmt.Errorf("R2 는 Cloudflare 계정 ID가 필요합니다")
		}
		// R2의 서명 리전은 늘 "auto"다. 다른 값을 넣으면 서명이 맞지 않는다.
		return "https://" + acct + ".r2.cloudflarestorage.com", "auto", nil
	case "s3":
		if ep := strings.TrimSpace(cfg.Endpoint); ep != "" {
			return ep, orDefault(region, "us-east-1"), nil
		}
		if region == "" {
			return "", "", fmt.Errorf("AWS S3 는 리전이 필요합니다 (예: ap-northeast-2)")
		}
		return "https://s3." + region + ".amazonaws.com", region, nil
	case "custom":
		ep := strings.TrimSpace(cfg.Endpoint)
		if ep == "" {
			return "", "", fmt.Errorf("S3 호환 스토리지는 엔드포인트 주소가 필요합니다")
		}
		return ep, orDefault(region, "us-east-1"), nil
	}
	return "", "", fmt.Errorf("알 수 없는 스토리지 종류입니다: %q", cfg.Provider)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Prefix는 백업 객체 이름 앞에 붙는 경로다.
func (o *ObjectStore) Prefix() string { return o.prefix }

// Describe는 화면에 보일 "어디에 올리는가"다.
func (o *ObjectStore) Describe() string {
	return o.scheme + "://" + o.host + "/" + o.bucket + "/" + o.prefix
}

func (o *ObjectStore) endpoint(key string, query url.Values) (*url.URL, string) {
	host := o.host
	path := "/"
	if o.virtual {
		host = o.bucket + "." + host
		path += key
	} else {
		path += o.bucket + "/" + key
	}
	u := &url.URL{Scheme: o.scheme, Host: host, Path: path}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return u, host
}

func (o *ObjectStore) newRequest(ctx context.Context, method, key string, query url.Values,
	body io.Reader) (*http.Request, error) {
	u, host := o.endpoint(key, query)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Host = host
	return req, nil
}

func (o *ObjectStore) send(req *http.Request, payloadHash string) (*http.Response, error) {
	if err := storage.SignV4(req, o.creds, payloadHash, o.signClock()); err != nil {
		return nil, err
	}
	res, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s 에 접속하지 못했습니다: %w", req.URL.Host, err)
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		return nil, objectError(res, o.bucket)
	}
	return res, nil
}

// objectError는 오류 응답을 사람 말로 바꾼다(storage.s3Error와 같은 이유).
func objectError(res *http.Response, bucket string) error {
	var payload struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	_ = xml.Unmarshal(body, &payload)
	switch payload.Code {
	case "SignatureDoesNotMatch":
		return fmt.Errorf("서명이 맞지 않습니다 — 시크릿 키를 확인하세요")
	case "InvalidAccessKeyId":
		return fmt.Errorf("액세스 키를 찾을 수 없습니다")
	case "AccessDenied":
		return fmt.Errorf("%s 버킷에 이 일을 할 권한이 없습니다 — 키의 읽기·쓰기 권한을 확인하세요", bucket)
	case "NoSuchBucket":
		return fmt.Errorf("%s 버킷이 없습니다", bucket)
	case "NoSuchKey":
		return fmt.Errorf("그 백업이 버킷에 없습니다")
	case "AuthorizationHeaderMalformed":
		return fmt.Errorf("리전이 맞지 않습니다: %s", strings.TrimSpace(payload.Message))
	}
	if payload.Message != "" {
		return fmt.Errorf("%s (%s)", strings.TrimSpace(payload.Message), res.Status)
	}
	return fmt.Errorf("요청이 거절됐습니다 (%s)", res.Status)
}

// Put은 파일 하나를 key로 올린다. meta는 x-amz-meta-* 머리글로 붙는다.
//
// 본문의 SHA-256을 먼저 계산해 서명에 넣는다. UNSIGNED-PAYLOAD로 보내면 계산을 아끼지만,
// 망 중간에서 바뀐 본문을 저장소가 그대로 받아 둔다 — 백업은 그런 것이 있으면 안 된다.
func (o *ObjectStore) Put(ctx context.Context, key, path, sha256Hex string, meta map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	req, err := o.newRequest(ctx, http.MethodPut, key, nil, f)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	for k, v := range meta {
		req.Header.Set("X-Amz-Meta-"+k, v)
	}
	res, err := o.send(req, sha256Hex)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// Get은 객체를 내려받는다. 돌려준 본문은 호출자가 닫는다. 두 번째 값은 x-amz-meta-* 다.
func (o *ObjectStore) Get(ctx context.Context, key string) (io.ReadCloser, map[string]string, error) {
	req, err := o.newRequest(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	res, err := o.send(req, "")
	if err != nil {
		return nil, nil, err
	}
	meta := map[string]string{}
	for name, vs := range res.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-meta-") && len(vs) > 0 {
			meta[strings.TrimPrefix(lower, "x-amz-meta-")] = vs[0]
		}
	}
	return res.Body, meta, nil
}

// Delete는 객체 하나를 지운다.
func (o *ObjectStore) Delete(ctx context.Context, key string) error {
	req, err := o.newRequest(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	res, err := o.send(req, "")
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// Object는 버킷 안의 객체 하나다.
type Object struct {
	Key        string    `json:"key"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

type listResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

// List는 접두사 아래의 객체를 모두 나열한다(최대 limit개).
func (o *ObjectStore) List(ctx context.Context, limit int) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("max-keys", strconv.Itoa(1000))
		if o.prefix != "" {
			q.Set("prefix", o.prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := o.newRequest(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, err
		}
		res, err := o.send(req, "")
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		var page listResult
		if err := xml.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("목록 응답을 해석하지 못했습니다: %w", err)
		}
		for _, c := range page.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size, ModifiedAt: c.LastModified})
		}
		if !page.IsTruncated || page.NextContinuationToken == "" || (limit > 0 && len(out) >= limit) {
			break
		}
		token = page.NextContinuationToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key > out[j].Key })
	return out, nil
}
