package api

import "testing"

// TestIsAssetPath는 SPA 폴백이 자산 요청을 삼키지 않는지 확인한다.
//
// 없는 자산에 index.html을 200으로 돌려주면 두 가지 문제가 생긴다.
//  1. 브라우저가 /favicon.ico를 자동 요청해 HTML을 받고, 그것을 아이콘으로 해석하려 한다
//  2. 없는 .js가 HTML로 응답되어 "HTML에서 문법 오류"라는 엉뚱한 오류가 보고된다
func TestIsAssetPath(t *testing.T) {
	assets := []string{
		"/favicon.ico",
		"/favicon.svg",
		"/apple-touch-icon.png",
		"/css/app.css",
		"/js/main.js",
		"/js/core/theme.js",
		"/js/pages/nosql.JS", // 대소문자 무관
		"/fonts/inter.woff2",
		"/site.webmanifest",
		"/robots.txt",
	}
	for _, p := range assets {
		if !isAssetPath(p) {
			t.Errorf("isAssetPath(%q) = false, 자산으로 판정해야 합니다", p)
		}
	}

	routes := []string{
		"/",
		"/connections",
		"/erd/9f2c1e5a-1111-2222-3333-444455556666",
		"/migrations/abc",
		"/assistant",
		"/users/1/access",
		// 점이 앞쪽 경로 요소에만 있는 경우는 라우트다.
		"/v1.2/users",
		"/erd/a.b/edit",
	}
	for _, p := range routes {
		if isAssetPath(p) {
			t.Errorf("isAssetPath(%q) = true, SPA 라우트로 판정해야 합니다", p)
		}
	}

	// 목록에 없는 확장자는 라우트로 본다 — 임의 확장자를 404로 만들면
	// 나중에 추가되는 라우트 형태를 예상 못하고 막을 수 있다.
	if isAssetPath("/report.pdf") {
		t.Error("알 수 없는 확장자를 자산으로 판정했습니다")
	}
}
