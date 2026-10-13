package erd

import "testing"

func TestResolveTint(t *testing.T) {
	cases := map[string]string{
		"빨강": "#ef4444", "red": "#ef4444", " RED ": "#ef4444", "#EF4444": "#ef4444",
		"회색": "#a1a1aa", "gray": "#a1a1aa",
		"": "", "기본": "", "default": "", "none": "",
	}
	for in, want := range cases {
		got, ok := ResolveTint(in)
		if !ok || got != want {
			t.Errorf("ResolveTint(%q) = %q, %v — %q 여야 합니다", in, got, ok, want)
		}
	}
	for _, bad := range []string{"#123456", "crimson", "빨간색"} {
		if _, ok := ResolveTint(bad); ok {
			t.Errorf("ResolveTint(%q) 가 통과했습니다. 팔레트에 없는 색은 고르개에 불이 들어오지 않는다", bad)
		}
	}
}

func TestIconValidation(t *testing.T) {
	if v, ok := ValidTableIcon("none"); !ok || v != "" {
		t.Errorf(`표 아이콘 "none" 은 없음("")이어야 합니다: %q %v`, v, ok)
	}
	if _, ok := ValidTableIcon("terminal"); ok {
		t.Error("terminal 은 컬럼 아이콘에만 있습니다")
	}
	if v, ok := ValidColumnIcon("auto"); !ok || v != "" {
		t.Errorf(`컬럼 아이콘 "auto" 는 자동("")이어야 합니다: %q %v`, v, ok)
	}
	if v, ok := ValidColumnIcon("NONE"); !ok || v != "none" {
		t.Errorf(`컬럼 아이콘 "none" 은 그대로 남아야 합니다(표시 안 함): %q %v`, v, ok)
	}
}
