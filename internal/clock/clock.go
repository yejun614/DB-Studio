// Package clock은 DB Studio가 스스로 관리하는 시계다.
//
// 왜 필요한가. TOTP는 서버와 인증 앱이 같은 시각을 본다는 가정 위에 서 있는데,
// 이 앱이 놓이는 자리(사내망 서버, 개발자 노트북, 컨테이너)의 시스템 시계는 자주
// 틀린다. NTP가 막혀 있거나, 가상 머신이 정지·재개되며 몇 분씩 밀리거나, 누군가
// 시각을 손으로 맞춰 놓는다. 그 상태에서 TOTP를 붙이면 증상은 "인증 앱이 고장 났다"로
// 나타나고, 사용자는 원인을 알 방법이 없다.
//
// 그래서 두 가지를 한다.
//
//  1. **단조 시계에 고정한다.** 시작할 때 벽시계를 한 번 읽어 기준으로 삼고, 그 뒤로는
//     경과 시간을 단조 시계로 잰다. 실행 중에 시스템 시각이 튀어도(NTP 도약, 수동 변경,
//     VM 재개) 우리 시계는 그만큼 튀지 않는다. time.Now()를 그대로 쓰면 그 순간
//     모든 사용자의 코드가 한꺼번에 틀린다.
//
//  2. **인증 앱에게서 배운다.** 기준으로 삼은 벽시계 자체가 틀렸을 수 있다. 그것을
//     알아낼 방법은 하나뿐이다 — 시각이 맞는 장치와 대조하는 것. 사용자의 인증 앱이
//     바로 그 장치다(휴대폰은 통신사 시각으로 동기화된다). TOTP 검증에 성공할 때마다
//     "몇 칸 어긋난 코드였는가"를 알 수 있으므로, 그 값을 조금씩 반영해 보정값을
//     좁혀 간다. 외부 시각 서버에 나가지 않고도 시계를 맞추는 셈이다.
//
// 보정값은 DB에 남긴다. 재시작할 때마다 처음부터 배우면 그 사이의 로그인이 실패한다.
package clock

import (
	"log/slog"
	"sync"
	"time"
)

// Clock은 내부 시각과 학습된 보정값을 들고 있다.
// 모든 메서드는 여러 고루틴에서 동시에 불릴 수 있다.
type Clock struct {
	mu sync.RWMutex

	// base는 시작 시점에 읽은 벽시계다(단조 시계 정보를 포함한다).
	// 경과 시간을 time.Since(base)로 재므로 시스템 시각이 바뀌어도 영향받지 않는다.
	base time.Time
	// offset은 "우리 기준 시각에 이만큼 더해야 진짜 시각"이라는 학습된 값이다.
	offset time.Duration
	// learned는 보정값이 실제 인증 성공에서 갱신된 횟수다. 화면에서
	// "아직 한 번도 맞춰 보지 않았다"와 "여러 번 확인했다"를 구분해 보여준다.
	learned int

	save func(offset time.Duration)
}

// maxOffset은 학습이 받아들이는 보정값의 한계다.
//
// 상한을 두는 이유: 보정값은 사용자의 입력에서 유도되므로, 한 사람의 잘못 맞춰진
// 휴대폰이 서버 시계를 몇 시간씩 끌고 가면 다른 모든 사람의 로그인이 깨진다.
// 하루를 넘는 오차는 시계 문제가 아니라 설정 문제이며, 사람이 봐야 한다.
const maxOffset = 24 * time.Hour

// New는 지금을 기준으로 시계를 만든다. offset은 저장소에서 읽어 온 지난 보정값이다.
func New(offset time.Duration) *Clock {
	return &Clock{base: time.Now(), offset: clamp(offset)}
}

// SetPersister는 보정값이 바뀔 때 불릴 저장 함수를 등록한다.
// 저장 실패가 인증을 막지 않도록 오류를 돌려받지 않는다 — 호출부가 로깅만 한다.
func (c *Clock) SetPersister(save func(offset time.Duration)) {
	c.mu.Lock()
	c.save = save
	c.mu.Unlock()
}

// Now는 보정을 반영한 내부 시각, 즉 **앱이 믿는 현재 시각**이다.
// 화면 표시와 새 등록의 출발점으로 쓴다.
//
// 단조 시계 정보는 떼어 낸다(Round(0)). 이 값은 저장되고 JSON으로 나가는데,
// 단조 정보가 붙은 시각은 비교 의미가 달라져 혼란을 부른다.
func (c *Clock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.base.Add(time.Since(c.base) + c.offset).Round(0).UTC()
}

// BaseNow는 보정을 **반영하지 않은** 기준 시각이다.
//
// 이것이 사용자별 보정값(user_totp.skew_seconds)의 기준점이다. 왜 Now가 아니라
// 이쪽에 매달아야 하는지가 중요하다: 전역 보정값은 다른 사람의 인증에서도 계속
// 움직인다. 이미 등록을 마친 사용자의 유효 시각이 그 움직임을 따라가면, 남이
// 로그인했다는 이유만으로 내 코드가 갑자기 틀리게 된다. 기준을 움직이지 않는
// 쪽에 두면 각자의 보정값은 각자의 것으로 남는다.
func (c *Clock) BaseNow() time.Time {
	c.mu.RLock()
	base := c.base
	c.mu.RUnlock()
	return base.Add(time.Since(base)).Round(0).UTC()
}

// Offset은 현재 보정값이다.
func (c *Clock) Offset() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.offset
}

// LearnedCount는 보정값이 갱신된 횟수다.
func (c *Clock) LearnedCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.learned
}

// SystemSkew는 내부 시각이 시스템 시각보다 얼마나 앞서 있는지다.
//
// 이 값이 크면 둘 중 하나다: 시스템 시계가 틀렸거나(학습된 보정값이 그것을 메우고 있다),
// 실행 중에 시스템 시각이 튀었다. 어느 쪽이든 운영자가 알아야 할 사실이므로
// 보안 설정 화면에 그대로 보여준다.
func (c *Clock) SystemSkew() time.Duration {
	return c.Now().Sub(time.Now().UTC())
}

// 가중치 상수. 관측을 얼마나 믿을지를 뜻한다.
//
// 등록 시점의 관측이 더 무거운 이유: 그 순간 사용자는 인증 앱을 보면서 방금 뜬
// 코드를 옮겨 적는다. 평소 로그인은 코드를 몇십 초 묵혀 두었다 넣는 경우가 섞이므로
// 조금씩만 반영해 흔들림을 줄인다.
const (
	WeightEnroll = 0.5
	WeightLogin  = 0.125
)

// Learn은 "기준 시각(BaseNow)에 correction을 더하면 진짜 시각"이라는 관측을 반영한다.
//
// 관측 하나를 그대로 믿지 않고 지수 평활로 조금씩 옮기는 이유: 이 값의 출처는
// 사용자의 인증 앱이고, 그중 하나는 시각이 틀려 있을 수 있다. 한 사람의 이상값이
// 전역 보정값을 통째로 끌고 가면 나머지 사람들의 등록이 어긋난다. 여러 관측이
// 쌓일수록 참값으로 수렴하고, 혼자 튀는 값은 weight배로 눌린다.
//
// 이 보정값은 **이미 등록을 마친 사용자에게 영향을 주지 않는다**(BaseNow 주석 참고).
// 쓰이는 곳은 새 등록의 출발점, 시각 재동기화 탐색의 중심, 그리고 운영자에게
// 보여줄 진단값이다.
func (c *Clock) Learn(correction time.Duration, weight float64) {
	if weight <= 0 {
		return
	}
	if weight > 1 {
		weight = 1
	}

	c.mu.Lock()
	before := c.offset
	c.offset = clamp(c.offset + time.Duration(float64(correction-c.offset)*weight))
	moved := c.offset != before
	if moved {
		c.learned++
	}
	save := c.save
	offset := c.offset
	c.mu.Unlock()

	if !moved {
		return
	}
	if save != nil {
		save(offset)
	}
	if abs(correction-before) > time.Minute {
		slog.Info("내부 시계 보정",
			"관측", correction.Round(time.Second),
			"전역보정", offset.Round(time.Second),
			"시스템과의차이", c.SystemSkew().Round(time.Second))
	}
}

func clamp(d time.Duration) time.Duration {
	if d > maxOffset {
		return maxOffset
	}
	if d < -maxOffset {
		return -maxOffset
	}
	return d
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// Status는 화면에 보여줄 시계 상태다.
type Status struct {
	InternalTime time.Time `json:"internalTime"`
	SystemTime   time.Time `json:"systemTime"`
	// OffsetSeconds는 학습된 보정값이다(초).
	OffsetSeconds int `json:"offsetSeconds"`
	// SkewSeconds는 내부 시각과 시스템 시각의 차이다(초).
	SkewSeconds int `json:"skewSeconds"`
	// Learned는 보정값이 갱신된 횟수다.
	Learned int `json:"learned"`
	// StartedAt은 기준 벽시계를 읽은 시각이다.
	StartedAt time.Time `json:"startedAt"`
}

func (c *Clock) Status() Status {
	c.mu.RLock()
	base := c.base
	c.mu.RUnlock()
	now := c.Now()
	return Status{
		InternalTime:  now,
		SystemTime:    time.Now().UTC(),
		OffsetSeconds: int(c.Offset().Round(time.Second) / time.Second),
		SkewSeconds:   int(now.Sub(time.Now().UTC()).Round(time.Second) / time.Second),
		Learned:       c.LearnedCount(),
		StartedAt:     base.Round(0).UTC(),
	}
}
