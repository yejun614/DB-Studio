package docker

import "testing"

// API 버전은 세마버가 아니다. 문자열로 견주면 1.43 이 1.5 보다 작게 나오고,
// 그러면 훨씬 새 데몬을 "너무 옛것"이라고 막는다.
func TestAPIAtLeast(t *testing.T) {
	cases := []struct {
		min, want string
		ok        bool
	}{
		{"1.24", "v1.43", true},
		{"1.43", "v1.43", true},
		{"1.44", "v1.43", false},
		// 문자열 비교로는 틀리는 조합. 1.43 은 1.5 보다 크다.
		{"1.5", "v1.43", true},
		{"1.43", "v1.5", false},
		{"2.0", "v1.43", false},
		{"1.43", "v2.0", true},
		// 데몬이 말해 주지 않으면 막지 않는다.
		{"", "v1.43", true},
		{"  ", "v1.43", true},
	}
	for _, c := range cases {
		if got := apiAtLeast(c.min, c.want); got != c.ok {
			t.Errorf("apiAtLeast(%q, %q) = %v, 기대 %v", c.min, c.want, got, c.ok)
		}
	}
}

func TestSplitAPIVersion(t *testing.T) {
	cases := map[string][2]int{
		"v1.43": {1, 43}, "1.43": {1, 43}, " 1.5 ": {1, 5},
		"2": {2, 0}, "": {0, 0}, "쓰레기": {0, 0},
	}
	for in, want := range cases {
		maj, min := splitAPIVersion(in)
		if maj != want[0] || min != want[1] {
			t.Errorf("splitAPIVersion(%q) = %d.%d, 기대 %d.%d", in, maj, min, want[0], want[1])
		}
	}
}
