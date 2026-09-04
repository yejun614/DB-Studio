package storage

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/opsapi"
)

// S3 규약 오브젝트 스토리지 클라이언트.
//
// **규약이지 서비스가 아니다.** AWS S3, MinIO, Ceph RGW, Wasabi, Cloudflare R2 가
// 모두 같은 REST 를 말한다. 그래서 종류를 서비스마다 만들지 않고 하나로 둔다 —
// 나뉘어 있으면 같은 코드가 이름만 달리해 다섯 벌이 되고, 새 호환 저장소가
// 나올 때마다 여섯 벌이 된다.
//
// **읽기 전용이다.** 버킷·객체 삭제는 되돌릴 수 없고, 이 화면은 "무엇이 어디에
// 얼마나 있는가"를 보는 자리다. 지우는 판단을 버튼 하나로 옮겨 놓는 것은 위험을
// 옮기는 것일 뿐이다(하둡·Ceph 화면과 같은 규칙이다).

const (
	// S3DefaultPort는 HTTPS 다. AWS 와 대부분의 호환 저장소가 이 포트를 쓴다.
	// MinIO 를 쓰는 사람은 9000 을 직접 적는다.
	S3DefaultPort = 443
	// s3MaxKeys는 한 번에 받아 오는 객체 수다. 목록 화면 한 장의 크기다.
	s3MaxKeys = 200
	// s3StatScanLimit은 버킷 크기를 어림잡을 때 훑는 객체 수의 상한이다.
	//
	// 상한이 필요한 이유: S3 에는 "이 버킷이 몇 바이트인가"를 묻는 API 가 없다.
	// 정확히 알려면 객체를 전부 나열해야 하는데, 수백만 개짜리 버킷에서 그것은
	// 화면 한 장을 여는 값으로 너무 크다. 그래서 여기까지만 세고, 더 있으면
	// "이 이상"이라고 말한다 — 조용히 틀린 합계를 보여주는 것보다 낫다.
	s3StatScanLimit = 5000
)

// S3는 S3 규약 저장소 클라이언트다.
type S3 struct {
	cfg    Config
	client *http.Client
}

func NewS3(cfg Config) *S3 {
	if _, ok := cfg.Extra["scheme"]; !ok {
		// 오브젝트 스토리지는 기본이 HTTPS 다. http 로 두면 첫 호출이 TLS 오류로
		// 끝나는데, 그 메시지는 원인을 짐작하기 어렵다(Ceph 와 같은 이유).
		cfg.Scheme = "https"
	}
	return &S3{cfg: cfg, client: cfg.HTTPClient()}
}

func (s *S3) Kind() string { return "s3" }

// Region은 서명에 쓸 리전이다.
func (s *S3) Region() string { return strings.TrimSpace(s.cfg.Extra["region"]) }

// pathStyle은 버킷을 경로에 둘지(/bucket/key) 호스트에 둘지(bucket.host/key)다.
//
// 경로가 기본인 이유: MinIO·Ceph RGW·개발용 서버는 대개 가상 호스트 이름을
// 풀 수 없다(DNS 에 bucket.minio-dev 가 없다). AWS 는 둘 다 받는다. 그래서
// 어디서나 되는 쪽을 기본으로 두고, 필요한 사람이 바꾼다.
func (s *S3) pathStyle() bool {
	return !strings.EqualFold(strings.TrimSpace(s.cfg.Extra["addressing"]), "virtual")
}

func (s *S3) creds() Creds {
	return Creds{
		AccessKey:    s.cfg.User,
		SecretKey:    s.cfg.Password,
		SessionToken: strings.TrimSpace(s.cfg.Extra["session_token"]),
		Region:       s.Region(),
		Service:      "s3",
	}
}

// endpoint는 버킷·키에 대한 요청 URL 을 만든다.
func (s *S3) endpoint(bucket, key string, query url.Values) (*url.URL, string) {
	host := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	// 기본 포트는 적지 않는다. 호스트 헤더에 :443 이 붙으면 서명이 서버 계산과
	// 어긋나는 구현이 있다.
	if (s.cfg.Scheme == "https" && s.cfg.Port == 443) ||
		(s.cfg.Scheme == "http" && s.cfg.Port == 80) {
		host = s.cfg.Host
	}
	path := "/"
	if bucket != "" {
		if s.pathStyle() {
			path = "/" + bucket
		} else {
			host = bucket + "." + host
		}
		if key != "" {
			if !strings.HasSuffix(path, "/") {
				path += "/"
			}
			path += key
		}
	}
	u := &url.URL{Scheme: s.cfg.Scheme, Host: host, Path: path}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return u, host
}

func (s *S3) do(ctx context.Context, method, bucket, key string, query url.Values, out any) error {
	u, host := s.endpoint(bucket, key, query)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return err
	}
	req.Host = host
	if err := SignV4(req, s.creds(), emptyPayloadHash, time.Now()); err != nil {
		return err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s 에 접속하지 못했습니다: %w", u.Host, err)
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		return s3Error(res, bucket)
	}
	if out == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if err := xml.Unmarshal(body, out); err != nil {
		return fmt.Errorf("응답을 해석하지 못했습니다: %w", err)
	}
	return nil
}

// s3Error는 오류 응답을 사람 말로 바꾼다.
//
// 상태 코드만 돌려주지 않는 이유: S3 의 403 은 "키가 틀렸다"와 "권한이 없다"와
// "서명이 어긋났다"가 모두 같은 숫자다. 본문의 Code 가 그 셋을 가른다.
func s3Error(res *http.Response, bucket string) error {
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
		if bucket != "" {
			return fmt.Errorf("%s 버킷에 접근할 권한이 없습니다", bucket)
		}
		return fmt.Errorf("접근할 권한이 없습니다")
	case "NoSuchBucket":
		return fmt.Errorf("%s 버킷이 없습니다", bucket)
	case "AuthorizationHeaderMalformed":
		// 이 오류의 본문에는 서버가 기대하는 리전이 들어 있다. 그것을 그대로
		// 전하는 것이 "리전 설정을 고치세요"보다 훨씬 빨리 끝난다.
		return fmt.Errorf("리전이 맞지 않습니다: %s", strings.TrimSpace(payload.Message))
	}
	if payload.Message != "" {
		return fmt.Errorf("%s (%s)", strings.TrimSpace(payload.Message), res.Status)
	}
	return fmt.Errorf("요청이 거절됐습니다 (%s)", res.Status)
}

// ---------- 목록 ----------

type listAllBuckets struct {
	Owner struct {
		DisplayName string `xml:"DisplayName"`
		ID          string `xml:"ID"`
	} `xml:"Owner"`
	Buckets struct {
		Bucket []struct {
			Name         string    `xml:"Name"`
			CreationDate time.Time `xml:"CreationDate"`
		} `xml:"Bucket"`
	} `xml:"Buckets"`
}

// Buckets는 버킷 목록이다. 크기·객체 수는 채우지 않는다 —
// 그것을 알려면 버킷마다 전부 나열해야 하고, 목록 한 장에 그 값을 치르지 않는다.
func (s *S3) Buckets(ctx context.Context) ([]Bucket, string, error) {
	var out listAllBuckets
	if err := s.do(ctx, http.MethodGet, "", "", nil, &out); err != nil {
		return nil, "", err
	}
	owner := out.Owner.DisplayName
	if owner == "" {
		owner = out.Owner.ID
	}
	buckets := make([]Bucket, 0, len(out.Buckets.Bucket))
	for _, b := range out.Buckets.Bucket {
		buckets = append(buckets, Bucket{Name: b.Name, Owner: owner})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })
	return buckets, owner, nil
}

// Object는 버킷 안의 객체 하나 또는 접두사(폴더처럼 보이는 것)다.
type Object struct {
	Key string `json:"key"`
	// Prefix가 참이면 이것은 객체가 아니라 접두사다(화면에서는 폴더로 보인다).
	Prefix       bool      `json:"prefix"`
	Size         int64     `json:"size"`
	ModifiedAt   time.Time `json:"modifiedAt"`
	ETag         string    `json:"etag,omitempty"`
	StorageClass string    `json:"storageClass,omitempty"`
}

// ObjectPage는 객체 목록 한 장이다.
type ObjectPage struct {
	Bucket    string   `json:"bucket"`
	Prefix    string   `json:"prefix"`
	Objects   []Object `json:"objects"`
	Truncated bool     `json:"truncated"`
	// Next는 다음 장을 요청할 때 그대로 돌려보내는 값이다. 비어 있으면 끝이다.
	Next string `json:"next,omitempty"`
}

type listObjectsV2 struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	CommonPrefixes        []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
		ETag         string    `xml:"ETag"`
		StorageClass string    `xml:"StorageClass"`
	} `xml:"Contents"`
}

// Objects는 한 버킷의 객체 목록 한 장이다.
//
// delimiter 를 늘 "/" 로 두는 이유: 수백만 개의 키를 평평하게 늘어놓으면 사람이
// 읽을 수 없다. 접두사로 접어 두면 파일 탐색기처럼 읽히고, 그것이 사람이 키
// 이름을 지을 때 실제로 의도한 구조다.
func (s *S3) Objects(ctx context.Context, bucket, prefix, token string) (*ObjectPage, error) {
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("버킷 이름이 필요합니다")
	}
	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("delimiter", "/")
	q.Set("max-keys", strconv.Itoa(s3MaxKeys))
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	var out listObjectsV2
	if err := s.do(ctx, http.MethodGet, bucket, "", q, &out); err != nil {
		return nil, err
	}
	page := &ObjectPage{
		Bucket: bucket, Prefix: prefix,
		Objects:   make([]Object, 0, len(out.Contents)+len(out.CommonPrefixes)),
		Truncated: out.IsTruncated, Next: out.NextContinuationToken,
	}
	// 접두사를 먼저 놓는다. 파일 탐색기와 같은 순서라 눈이 헤매지 않는다.
	for _, p := range out.CommonPrefixes {
		page.Objects = append(page.Objects, Object{Key: p.Prefix, Prefix: true})
	}
	for _, c := range out.Contents {
		// 접두사 자신(예: "logs/")이 0바이트 객체로 함께 오는 저장소가 있다.
		// 그대로 두면 폴더와 같은 이름의 빈 파일이 나란히 보인다.
		if c.Key == prefix {
			continue
		}
		page.Objects = append(page.Objects, Object{
			Key: c.Key, Size: c.Size, ModifiedAt: c.LastModified,
			ETag:         strings.Trim(c.ETag, `"`),
			StorageClass: c.StorageClass,
		})
	}
	return page, nil
}

// BucketStat은 버킷 하나를 훑어 얻은 어림값이다.
type BucketStat struct {
	Bucket  string `json:"bucket"`
	Objects int64  `json:"objects"`
	Size    int64  `json:"size"`
	// Partial이 참이면 상한까지만 세었다는 뜻이다. 화면은 "이 이상"으로 적는다.
	Partial bool `json:"partial"`
}

// Stat은 버킷 하나의 객체 수와 크기를 어림잡는다.
//
// S3 에는 이것을 묻는 API 가 없어서 나열해 세는 수밖에 없다. 상한을 두고, 넘으면
// Partial 로 알린다 — 정확한 값처럼 보이는 틀린 합계보다 "여기까지 세었다"가 낫다.
func (s *S3) Stat(ctx context.Context, bucket string) (*BucketStat, error) {
	stat := &BucketStat{Bucket: bucket}
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("max-keys", strconv.Itoa(1000))
		if token != "" {
			q.Set("continuation-token", token)
		}
		var out listObjectsV2
		if err := s.do(ctx, http.MethodGet, bucket, "", q, &out); err != nil {
			return nil, err
		}
		for _, c := range out.Contents {
			stat.Objects++
			stat.Size += c.Size
		}
		if !out.IsTruncated || out.NextContinuationToken == "" {
			return stat, nil
		}
		if stat.Objects >= s3StatScanLimit {
			stat.Partial = true
			return stat, nil
		}
		token = out.NextContinuationToken
		if err := ctx.Err(); err != nil {
			stat.Partial = true
			return stat, nil
		}
	}
}

// Ping은 접속과 자격증명을 확인한다. 버킷 목록이 가장 값싼 인증 호출이다.
func (s *S3) Ping(ctx context.Context) (string, error) {
	if _, _, err := s.Buckets(ctx); err != nil {
		return "", err
	}
	// S3 규약에는 "서버 버전"이 없다. 무엇에 붙었는지를 대신 말한다.
	region := s.Region()
	if region == "" {
		region = "리전 미지정"
	}
	return "S3 호환 (" + region + ")", nil
}

// Overview는 개요 화면 한 장이다.
func (s *S3) Overview(ctx context.Context) (*Overview, error) {
	buckets, owner, err := s.Buckets(ctx)
	if err != nil {
		return nil, err
	}
	ov := &Overview{Kind: "s3",
		Health: Health{Level: HealthOK, Summary: fmt.Sprintf("버킷 %d개", len(buckets))}}
	ov.Version, _ = s.Ping(ctx)
	ov.Facts = []Fact{
		{Label: "버킷", Value: strconv.Itoa(len(buckets))},
		{Label: "엔드포인트", Value: s.cfg.BaseURL()},
	}
	if owner != "" {
		ov.Facts = append(ov.Facts, Fact{Label: "소유자", Value: owner})
	}
	if r := s.Region(); r != "" {
		ov.Facts = append(ov.Facts, Fact{Label: "리전", Value: r})
	}
	if !s.pathStyle() {
		ov.Facts = append(ov.Facts, Fact{Label: "주소 방식", Value: "가상 호스트"})
	}
	// 용량은 비워 둔다. 오브젝트 스토리지에는 "총 용량"이라는 것이 없고(쓰는 만큼
	// 늘어난다), 있는 척 0을 채우면 사용률 막대가 늘 0%로 보인다.
	ov.Notes = append(ov.Notes,
		"오브젝트 스토리지에는 총 용량이라는 개념이 없어 사용률을 보여주지 않습니다. "+
			"버킷 크기는 버킷을 고르면 훑어서 어림잡습니다")
	return ov, nil
}

// S3Metrics는 지표 수집 결과다.
type S3Metrics struct {
	Buckets int
	UpMS    float64
}

// Collect는 모니터링 지표를 모은다.
//
// 객체 수와 크기를 여기서 세지 않는 이유: 수집은 30초마다 도는데, 버킷마다
// 나열하면 그 주기가 저장소에 그대로 부하로 간다. 정기적으로 알아야 할 것은
// "붙는가"와 "버킷이 몇 개인가"이고, 크기는 사람이 화면에서 물을 때 센다.
func (s *S3) Collect(ctx context.Context) (*S3Metrics, error) {
	start := time.Now()
	buckets, _, err := s.Buckets(ctx)
	if err != nil {
		return nil, err
	}
	return &S3Metrics{
		Buckets: len(buckets),
		UpMS:    float64(time.Since(start).Microseconds()) / 1000,
	}, nil
}

// 이 파일이 opsapi 를 쓰는 것을 컴파일러에 알린다(Config·HTTPClient 별칭).
var _ = opsapi.HumanBytes
