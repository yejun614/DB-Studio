package erd

import (
	"slices"
	"strings"
)

// 도면의 겉모습(색·아이콘)으로 고를 수 있는 값.
//
// 그리는 쪽(web/js/core/tints.js, web/js/pages/erdeditor.js 의 TABLE_ICONS,
// web/js/core/colicon.js 의 COLUMN_ICONS)과 **같은 목록**이어야 한다. 서버가 이 목록을
// 아는 이유는 화면을 거치지 않는 길(AI 툴·MCP)로 들어온 값을 거르기 위해서다. 팔레트에
// 없는 색을 저장하면 카드는 칠해지지만 색 고르개의 어느 칸에도 불이 들어오지 않아, 사람은
// 그 색을 다시 고를 수도 "지금 무슨 색인지" 알 수도 없다. 두 벌의 어긋남은 저장소 뿌리의
// appearance_lists_test.go 가 잡는다.

// Tint는 색 하나다. Value 가 문서에 저장되는 값이다.
type Tint struct {
	Value string `json:"value"`
	Label string `json:"label"`
	// Name은 영어 이름이다. 모델은 "red"로도 말한다.
	Name string `json:"name"`
}

// Tints는 색상환 차례의 팔레트다(tints.js 의 TINTS 와 같은 순서·같은 값).
var Tints = []Tint{
	{"#ef4444", "빨강", "red"},
	{"#f97316", "주황", "orange"},
	{"#eab308", "노랑", "yellow"},
	{"#84cc16", "연두", "lime"},
	{"#22c55e", "초록", "green"},
	{"#14b8a6", "청록", "teal"},
	{"#0ea5e9", "하늘", "sky"},
	{"#3b82f6", "파랑", "blue"},
	{"#6366f1", "남색", "indigo"},
	{"#a855f7", "보라", "purple"},
	{"#d946ef", "자주", "fuchsia"},
	{"#ec4899", "분홍", "pink"},
	{"#b45309", "갈색", "brown"},
	{"#a1a1aa", "회색", "gray"},
}

// TableIcons는 표 카드 제목에 붙일 수 있는 아이콘이다(빈 값 = 없음).
var TableIcons = []string{
	"", "users", "key", "database", "activity", "list", "lock", "settings", "history",
	"cart", "box", "money", "chat", "mail", "bell", "calendar", "chart",
	"file", "tag", "location", "truck", "star", "flag", "shield", "code", "link",
}

// ColumnIcons는 컬럼에 고를 수 있는 아이콘이다. 이 밖에 "none"(표시 안 함)과
// 빈 값(타입·키를 보고 자동)이 있다.
var ColumnIcons = []string{
	"key", "link", "tag", "file", "list", "check", "chart", "money",
	"calendar", "history", "box", "code", "lock", "shield", "mail", "chat",
	"bell", "location", "truck", "cart", "star", "flag", "users", "database",
	"activity", "settings", "table", "terminal",
}

// ResolveTint는 사람·모델이 적은 색을 저장할 값으로 바꾼다. 팔레트의 값(대소문자 무시),
// 한국어 이름, 영어 이름을 받고, "기본"·"default"·"none"·빈 값은 색 없음("")이다.
// 팔레트에 없으면 ok=false 다.
func ResolveTint(s string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "", "기본", "default", "none", "없음":
		return "", true
	}
	for _, t := range Tints {
		if v == t.Value || v == t.Name || v == strings.ToLower(t.Label) {
			return t.Value, true
		}
	}
	return "", false
}

// ValidTableIcon은 표 아이콘으로 쓸 수 있는 값인지 본다("none" 은 없음과 같다).
func ValidTableIcon(s string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "none" {
		v = ""
	}
	if slices.Contains(TableIcons, v) {
		return v, true
	}
	return "", false
}

// ValidColumnIcon은 컬럼 아이콘으로 쓸 수 있는 값인지 본다. "auto"·빈 값은 자동(""),
// "none" 은 표시 안 함이다.
func ValidColumnIcon(s string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "", "auto":
		return "", true
	case "none":
		return "none", true
	}
	if slices.Contains(ColumnIcons, v) {
		return v, true
	}
	return "", false
}
