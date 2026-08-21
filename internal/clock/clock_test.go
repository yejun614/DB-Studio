package clock

import (
	"testing"
	"time"
)

func TestNowAppliesOffset(t *testing.T) {
	c := New(90 * time.Second)
	skew := c.Now().Sub(time.Now().UTC())
	// 실행 시간만큼의 오차는 허용한다. 확인하려는 것은 보정값이 반영되었는지다.
	if skew < 89*time.Second || skew > 91*time.Second {
		t.Errorf("보정값이 반영되지 않았습니다: skew = %v", skew)
	}
}

func TestNewClampsStoredOffset(t *testing.T) {
	// 저장된 값이 손상되었더라도 시계가 상식 밖으로 끌려가면 안 된다.
	if got := New(400 * time.Hour).Offset(); got != maxOffset {
		t.Errorf("offset = %v, want %v", got, maxOffset)
	}
	if got := New(-400 * time.Hour).Offset(); got != -maxOffset {
		t.Errorf("offset = %v, want %v", got, -maxOffset)
	}
}

// BaseNow는 전역 보정값과 무관해야 한다.
// 이미 등록을 마친 사용자의 보정값이 이것을 기준으로 하므로, 여기가 흔들리면
// 남의 로그인 때문에 내 코드가 틀리게 된다.
func TestBaseNowIgnoresOffset(t *testing.T) {
	c := New(0)
	before := c.BaseNow()
	c.Learn(10*time.Minute, 1)
	after := c.BaseNow()

	if drift := after.Sub(before); drift > time.Second {
		t.Errorf("전역 보정이 기준 시각을 움직였습니다: %v", drift)
	}
	if skew := c.Now().Sub(c.BaseNow()); skew < 9*time.Minute || skew > 11*time.Minute {
		t.Errorf("Now와 BaseNow의 차이 = %v, want ≈10m", skew)
	}
}

// Learn은 관측값을 그대로 믿지 않고 가중치만큼만 옮긴다.
func TestLearnMovesByWeight(t *testing.T) {
	c := New(0)
	c.Learn(60*time.Second, 0.25)
	if got := c.Offset(); got != 15*time.Second {
		t.Errorf("보정값 = %v, want 15s (60s의 1/4)", got)
	}
	// 이미 보정값이 있으면 그 차이만큼만 움직인다.
	c.Learn(15*time.Second, 0.5)
	if got := c.Offset(); got != 15*time.Second {
		t.Errorf("같은 값을 관측했는데 움직였습니다: %v", got)
	}
}

func TestLearnConvergesOnRepeatedObservations(t *testing.T) {
	c := New(0)
	// 같은 오차가 계속 관측되면 보정값이 그쪽으로 수렴해야 한다.
	// (여러 사용자의 인증 앱이 "서버가 2분 느리다"고 알려 주는 상황이다.)
	target := 120 * time.Second
	for i := 0; i < 60; i++ {
		c.Learn(target, WeightLogin)
	}
	if diff := target - c.Offset(); diff > 2*time.Second || diff < -2*time.Second {
		t.Errorf("수렴하지 않았습니다: offset = %v, want ≈ %v", c.Offset(), target)
	}
	if c.LearnedCount() == 0 {
		t.Error("학습 횟수가 기록되지 않았습니다")
	}
}

// 한 사람의 이상값이 전역 보정값을 통째로 끌고 가면 안 된다.
func TestLearnDampensOutlier(t *testing.T) {
	c := New(0)
	for i := 0; i < 20; i++ {
		c.Learn(0, WeightLogin) // 시계가 맞는 사용자들
	}
	c.Learn(30*time.Minute, WeightLogin) // 휴대폰 시각이 혼자 틀린 사용자
	if got := c.Offset(); got > 4*time.Minute {
		t.Errorf("이상값 하나에 보정값이 %v 움직였습니다", got)
	}
}

func TestLearnIgnoresZeroWeight(t *testing.T) {
	c := New(0)
	c.Learn(30*time.Second, 0)
	if c.Offset() != 0 {
		t.Errorf("가중치 0인데 보정값이 움직였습니다: %v", c.Offset())
	}
}

func TestLearnRespectsClamp(t *testing.T) {
	c := New(maxOffset)
	c.Learn(100*time.Hour, 1)
	if c.Offset() != maxOffset {
		t.Errorf("상한을 넘었습니다: %v", c.Offset())
	}
}

func TestPersisterCalledOnChange(t *testing.T) {
	c := New(0)
	var saved []time.Duration
	c.SetPersister(func(d time.Duration) { saved = append(saved, d) })

	c.Learn(40*time.Second, 0.5)
	c.Learn(20*time.Second, 0.5) // 이미 그 값이므로 변화 없음 → 저장하지 않는다

	if len(saved) != 1 {
		t.Fatalf("저장 호출 %d회, want 1회", len(saved))
	}
	if saved[0] != 20*time.Second {
		t.Errorf("저장된 보정값 = %v, want 20s", saved[0])
	}
}

func TestStatus(t *testing.T) {
	c := New(30 * time.Second)
	s := c.Status()
	if s.OffsetSeconds != 30 {
		t.Errorf("OffsetSeconds = %d, want 30", s.OffsetSeconds)
	}
	if s.SkewSeconds < 29 || s.SkewSeconds > 31 {
		t.Errorf("SkewSeconds = %d, want ≈30", s.SkewSeconds)
	}
	if s.InternalTime.IsZero() || s.StartedAt.IsZero() {
		t.Error("시각 필드가 비어 있습니다")
	}
}
