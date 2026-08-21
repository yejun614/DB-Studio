package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	// 이미지 헤더를 읽기 위한 디코더 등록. 실제 디코딩은 하지 않고 DecodeConfig만 쓴다.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 프로필 이미지 업로드.
//
// 세 가지를 확인하고 나서야 저장한다.
//
//  1. **크기.** -avatar-max-kb 보다 크면 거부한다. 읽기 전에 Content-Length를 보고,
//     읽는 중에도 상한을 넘으면 끊는다(헤더는 거짓말할 수 있다).
//  2. **실제 형식.** 확장자나 Content-Type이 아니라 바이트를 디코딩해 확인한다.
//     .png로 올린 실행 파일은 png가 아니고, 그것을 이미지로 서빙하면 안 된다.
//  3. **SVG는 받지 않는다.** SVG는 스크립트를 품을 수 있는 문서 형식이다. 사용자가
//     올린 것을 다른 사용자에게 보여주는 자리에 두면 저장형 XSS와 같은 말이 된다.
//
// URI 가져오기는 서버가 한 번 내려받아 같은 검사를 거친 뒤 저장한다.
// URL을 그대로 두고 브라우저가 불러오게 하면 CSP(img-src 'self' data:)를 열어야 하고,
// 링크가 깨지거나 외부망이 없으면 아바타가 사라진다. 대신 SSRF를 막기 위해
// 사설망·루프백 주소를 기본으로 차단한다.

// avatarFetchTimeout은 URI 가져오기의 시간 상한이다.
const avatarFetchTimeout = 15 * time.Second

func (s *Server) avatarMaxBytes() int64 {
	kb := s.cfg.AvatarMaxKB
	if kb <= 0 {
		kb = 512
	}
	return int64(kb) * 1024
}

// handleUploadAvatar는 multipart 업로드를 받는다.
func (s *Server) handleUploadAvatar(c *fiber.Ctx) error {
	u := currentUser(c)

	header, err := c.FormFile("file")
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "이미지 파일을 첨부하세요")
	}
	maxBytes := s.avatarMaxBytes()
	if header.Size > maxBytes {
		return fail(c, fiber.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("이미지는 %dKB 이하여야 합니다 (올린 파일 %dKB)",
				maxBytes/1024, header.Size/1024))
	}

	file, err := header.Open()
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "파일을 읽지 못했습니다")
	}
	defer file.Close()

	// 상한보다 1바이트 더 읽는다. 정확히 상한만 읽으면 "딱 맞는 파일"과
	// "넘치는 파일"을 구분할 수 없다.
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "파일을 읽지 못했습니다")
	}
	if int64(len(data)) > maxBytes {
		return fail(c, fiber.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("이미지는 %dKB 이하여야 합니다", maxBytes/1024))
	}

	return s.storeAvatar(c, u, data, "upload", "")
}

type avatarURIRequest struct {
	URI string `json:"uri"`
}

// handleImportAvatarURI는 URI에서 이미지를 내려받아 저장한다.
func (s *Server) handleImportAvatarURI(c *fiber.Ctx) error {
	u := currentUser(c)

	var req avatarURIRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	raw := strings.TrimSpace(req.URI)
	if raw == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "이미지 주소를 입력하세요")
	}

	// data: URI는 내려받을 것이 없다. 브라우저에서 파일을 붙여넣기로 옮길 때
	// 흔히 생기는 형태이므로 그대로 받아 준다.
	if strings.HasPrefix(raw, "data:") {
		data, err := decodeDataURI(raw, s.avatarMaxBytes())
		if err != nil {
			return fail(c, fiber.StatusBadRequest, "invalid_uri", err.Error())
		}
		return s.storeAvatar(c, u, data, "uri", "data:")
	}

	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fail(c, fiber.StatusBadRequest, "invalid_uri",
			"http 또는 https 주소만 사용할 수 있습니다")
	}

	data, err := s.fetchAvatar(c.Context(), parsed)
	if err != nil {
		return failDetail(c, fiber.StatusBadRequest, "fetch_failed",
			"이미지를 가져오지 못했습니다", err.Error())
	}
	return s.storeAvatar(c, u, data, "uri", raw)
}

// fetchAvatar는 주소에서 이미지를 내려받는다.
func (s *Server) fetchAvatar(ctx context.Context, target *url.URL) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, avatarFetchTimeout)
	defer cancel()

	client := &http.Client{
		Timeout: avatarFetchTimeout,
		// 리다이렉트를 따라가되 횟수를 제한한다. 무제한이면 리다이렉트 루프에
		// 걸리고, 아예 막으면 CDN 주소 대부분이 실패한다.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("리다이렉트가 너무 많습니다")
			}
			// 리다이렉트 대상도 검사한다. 공개 주소로 시작해 사설망으로 보내는 것이
			// SSRF의 전형적인 수법이다.
			return s.checkAvatarHost(req.URL, req.Context())
		},
	}
	if err := s.checkAvatarHost(target, ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dbstudio-avatar-fetch")
	req.Header.Set("Accept", strings.Join(model.AvatarMimes(), ", "))

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("응답 코드 %d", res.StatusCode)
	}
	maxBytes := s.avatarMaxBytes()
	if res.ContentLength > maxBytes {
		return nil, fmt.Errorf("이미지가 %dKB를 넘습니다", maxBytes/1024)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("이미지가 %dKB를 넘습니다", maxBytes/1024)
	}
	return data, nil
}

// checkAvatarHost는 대상 주소가 내려받아도 되는 곳인지 확인한다.
//
// 이 검사가 없으면 아바타 가져오기가 내부망 스캐너가 된다. 아바타는 모든 사용자가
// 쓸 수 있는 기능이라(커넥션 등록은 어드민 전용이다) 특히 넓게 열려 있다.
func (s *Server) checkAvatarHost(target *url.URL, ctx context.Context) error {
	if s.cfg.AvatarAllowPrivateURI {
		return nil
	}
	host := target.Hostname()
	if host == "" {
		return fmt.Errorf("주소에 호스트가 없습니다")
	}

	var resolver net.Resolver
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("호스트 이름을 찾을 수 없습니다: %s", host)
	}
	for _, addr := range addrs {
		ip := addr.IP
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("사설망·루프백 주소에서는 이미지를 가져올 수 없습니다 (%s)", ip)
		}
	}
	return nil
}

// decodeDataURI는 data: URI에서 바이트를 꺼낸다.
func decodeDataURI(raw string, maxBytes int64) ([]byte, error) {
	_, payload, found := strings.Cut(raw, ",")
	if !found {
		return nil, fmt.Errorf("data URI 형식이 올바르지 않습니다")
	}
	header := raw[:strings.Index(raw, ",")]
	if !strings.Contains(header, ";base64") {
		return nil, fmt.Errorf("base64 인코딩된 data URI만 지원합니다")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("data URI를 해석할 수 없습니다")
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("이미지가 %dKB를 넘습니다", maxBytes/1024)
	}
	return data, nil
}

// storeAvatar는 형식을 확인하고 저장한다.
func (s *Server) storeAvatar(c *fiber.Ctx, u *model.User, data []byte, source, uri string) error {
	if len(data) == 0 {
		return fail(c, fiber.StatusBadRequest, "bad_request", "빈 파일입니다")
	}

	// 바이트를 디코딩해 실제 형식을 확인한다. Content-Type과 확장자는 믿지 않는다.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_image",
			"이미지 형식을 알아볼 수 없습니다. PNG·JPEG·GIF·WebP만 지원합니다")
	}
	mime := "image/" + format
	if format == "jpeg" {
		mime = "image/jpeg"
	}
	if !model.ValidAvatarMime(mime) {
		return fail(c, fiber.StatusBadRequest, "invalid_image",
			"지원하지 않는 이미지 형식입니다: "+format)
	}
	// 화면에서 아바타는 최대 64px로 그려진다. 거대한 이미지는 저장 공간과 전송량만
	// 쓰므로 상한을 둔다. 리사이즈를 하지 않는 이유는 그 순간 이미지 처리 라이브러리와
	// 그 취약점을 떠안게 되기 때문이다 — 거부하는 편이 정직하다.
	const maxDimension = 2048
	if cfg.Width > maxDimension || cfg.Height > maxDimension {
		return fail(c, fiber.StatusBadRequest, "too_large",
			fmt.Sprintf("이미지 크기는 %dx%d 이하여야 합니다 (올린 이미지 %dx%d)",
				maxDimension, maxDimension, cfg.Width, cfg.Height))
	}

	version, err := s.st.SaveUserAvatar(c.Context(), store.SaveAvatarParams{
		UserID: u.ID, Mime: mime, Bytes: data,
		Width: cfg.Width, Height: cfg.Height, Source: source, SourceURI: uri,
	})
	if err != nil {
		return err
	}

	s.audit(c, store.AuditParams{
		Action: store.ActionProfileUpdated, TargetType: "user", TargetID: u.ID,
		Detail: map[string]any{
			"fields": []string{"avatar"}, "source": source, "mime": mime,
			"bytes": len(data), "size": fmt.Sprintf("%dx%d", cfg.Width, cfg.Height),
			"uri": uri,
		},
	})

	updated, err := s.st.GetUser(c.Context(), u.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "user": updated, "version": version})
}

// handleDeleteAvatar는 업로드한 이미지를 지운다.
func (s *Server) handleDeleteAvatar(c *fiber.Ctx) error {
	u := currentUser(c)
	if err := s.st.DeleteUserAvatar(c.Context(), u.ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionProfileUpdated, TargetType: "user", TargetID: u.ID,
		Detail: map[string]any{"fields": []string{"avatar"}, "source": "removed"},
	})
	updated, err := s.st.GetUser(c.Context(), u.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "user": updated})
}

// handleGetAvatar는 이미지를 서빙한다.
//
// 인증된 사용자만 볼 수 있다. 아바타는 민감한 자료는 아니지만, 로그인 없이
// /users/<id>/avatar 로 사용자 ID의 존재 여부를 확인할 수 있게 둘 이유가 없다.
func (s *Server) handleGetAvatar(c *fiber.Ctx) error {
	img, err := s.st.GetUserAvatar(c.Context(), c.Params("id"), true)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "프로필 이미지가 없습니다")
	}
	if err != nil {
		return err
	}

	c.Set(fiber.HeaderContentType, img.Mime)
	c.Set("Content-Length", strconv.Itoa(len(img.Bytes)))
	// 버전이 URL에 실려 오므로 오래 캐시해도 안전하다. private로 두는 이유는
	// 프록시가 다른 사용자에게 같은 이미지를 돌려주지 않게 하기 위함이다.
	c.Set("Cache-Control", "private, max-age=86400")
	// 이미지가 문서로 해석되는 경로를 막는다. 서버가 형식을 확인했지만,
	// 그것과 별개로 브라우저가 다른 것으로 추측하게 두지 않는다.
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Content-Disposition", "inline")
	return c.Send(img.Bytes)
}
