package provision

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"dbstudio/internal/applog"
	"dbstudio/internal/docker"
	"dbstudio/internal/store"
)

// 만들기·멈추기·다시 시작하기.
//
// ── 왜 비동기인가 ───────────────────────────────────────────────────
// 이미지를 내려받는 데 몇 분이 걸린다(ClickHouse 는 압축해서 300MB 를 넘는다).
// HTTP 요청 하나를 그만큼 잡고 있으면 프록시가 먼저 끊고, 끊긴 뒤에도 도커는
// 계속 내려받는다 — 사람에게는 "실패했다"로 보이는데 실제로는 만들어진다.
//
// 그래서 백업과 같은 모양을 쓴다: 줄을 먼저 적고 id 를 돌려주고, 진행 상황은
// 그 줄의 한 칸(progress)에 덮어쓴다. 스트리밍하지 않는 이유도 같다 —
// 이 작업이 만드는 정보는 "지금 어디까지 왔는가" 하나뿐이라 덮어쓰면 된다.
//
// ── 왜 우리 표를 먼저 적는가 ────────────────────────────────────────
// 도커에 먼저 만들면, 그 사이에 앱이 죽으면 우리가 모르는 컨테이너가 남는다.
// 라벨이 붙어 있으니 찾을 수는 있지만 어느 프로젝트의 것인지·누가 만들었는지는
// 잃는다. 우리 줄이 먼저 있으면 그 반대다 — 실패한 줄이 남고, 그것은 화면에서
// 보이고 지울 수 있다.

// Store는 러너가 쓰는 저장소의 좁은 얼굴이다.
//
// 좁게 두는 이유는 이 저장소의 관례와 같다(macro.Backuper 처럼). 러너가
// store.Store 전체를 알면 검사에서 SQLite 를 띄워야 하고, 그러면 "계획대로
// 도커를 부르는가"를 확인하려고 마이그레이션을 돌리게 된다.
type Store interface {
	GetDBInstance(ctx context.Context, id string) (*store.DBInstance, error)
	UpdateDBInstance(ctx context.Context, id string, p store.UpdateDBInstanceParams) error
}

// Runner는 도커를 실제로 부르는 쪽이다.
type Runner struct {
	docker  *docker.Client
	st      Store
	timeout time.Duration

	// running은 지금 만들고 있는 인스턴스다.
	//
	// 두 번 누르는 것을 막는 자리다. 같은 인스턴스를 두 번 만들면 도커가
	// 409(이름 충돌)를 주는데, 그 실패가 첫 번째 시도의 진행 중에 끼어들어
	// 상태를 failed 로 덮는다 — 실제로는 잘 되고 있는 것을 실패로 적는 셈이다.
	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// NewRunner는 러너를 만든다.
func NewRunner(cl *docker.Client, st Store, timeout time.Duration) *Runner {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Runner{docker: cl, st: st, timeout: timeout, running: map[string]context.CancelFunc{}}
}

// ErrBusy는 그 인스턴스에 이미 작업이 돌고 있다는 뜻이다.
var ErrBusy = errors.New("이 DB 에 이미 작업이 돌고 있습니다")

// Create는 컨테이너를 만들고 시작한다. **바로 돌아온다.**
//
// 진행 상황은 인스턴스의 progress·status 에 적힌다. 화면은 그것을 되풀어 읽는다.
//
// onReady 는 DB 가 떠서 쓸 수 있게 된 뒤에 부른다(nil 이면 아무 일도 없다).
// 뜬 DB 를 커넥션으로 등록하는 자리다. 이 꾸러미가 직접 등록하지 않는 이유:
// 등록은 프로젝트·서버·접근 등급을 아는 쪽(api)의 몫이고, 여기서 하면 도커와
// 커넥션이 한 덩어리가 된다. 그리고 **뜬 뒤에** 해야 한다 — 포트는 뜨고 나서야
// 정해지고, 아직 안 뜬 DB 를 등록하면 접속 확인이 실패해서 사람이 자기 설정을
// 잘못한 것으로 읽는다.
func (r *Runner) Create(instanceID string, plan *Plan, onReady func(string)) error {
	r.mu.Lock()
	if _, busy := r.running[instanceID]; busy {
		r.mu.Unlock()
		return ErrBusy
	}
	// 배경 작업이므로 요청의 ctx 에 매달지 않는다. 요청이 끝나도 계속돼야 한다.
	//
	// 시간 상한은 넉넉히 둔다. 이미지 내려받기가 대부분이고, 느린 회선에서
	// 300MB 를 받는 데 십 분이 걸리는 일이 있다.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	r.running[instanceID] = cancel
	r.mu.Unlock()

	go func() {
		defer applog.Recover("provision.create")
		defer func() {
			cancel()
			r.mu.Lock()
			delete(r.running, instanceID)
			r.mu.Unlock()
		}()
		if err := r.create(ctx, instanceID, plan); err != nil {
			r.fail(instanceID, err)
			return
		}
		// 등록이 실패해도 만든 것은 만든 것이다. 여기서 상태를 failed 로
		// 돌리면 잘 돌고 있는 DB 가 실패로 보이고, 사람은 그것을 지운다.
		if onReady != nil {
			onReady(instanceID)
		}
	}()
	return nil
}

// Cancel은 도는 작업을 멈춘다.
func (r *Runner) Cancel(instanceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cancel, ok := r.running[instanceID]
	if ok {
		cancel()
	}
	return ok
}

// Busy는 그 인스턴스에 작업이 돌고 있는지다.
func (r *Runner) Busy(instanceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.running[instanceID]
	return ok
}

func (r *Runner) create(ctx context.Context, id string, plan *Plan) error {
	r.progress(id, "네트워크를 준비합니다")
	if err := r.docker.EnsureNetwork(ctx, NetworkName, NetworkLabels()); err != nil {
		return err
	}

	if plan.Volume != "" {
		r.progress(id, "볼륨을 준비합니다")
		if err := r.docker.CreateVolume(ctx, plan.Volume, plan.Labels(id)); err != nil {
			return err
		}
	}

	// 이미지가 이미 있으면 내려받지 않는다. 있는데도 부르면 도커가 레지스트리에
	// 물어보러 가고, 인터넷이 없는 기계에서는 그것만으로 실패한다.
	have, err := r.docker.ImageExists(ctx, plan.Image)
	if err != nil {
		return err
	}
	if !have {
		r.progress(id, "이미지를 내려받습니다: "+plan.Image)
		last := time.Now()
		err := r.docker.PullImage(ctx, plan.Image, func(p docker.PullProgress) {
			// 진행률을 매 줄 적지 않는다. 도커는 초당 수십 줄을 주고, 그만큼
			// UPDATE 를 돌리면 메타 DB 가 그것으로만 바쁘다.
			if time.Since(last) < time.Second {
				return
			}
			last = time.Now()
			r.progress(id, "이미지를 내려받습니다: "+pullLine(plan.Image, p))
		})
		if err != nil {
			return err
		}
	}

	// 같은 이름의 컨테이너가 남아 있으면 치운다.
	//
	// 왜 필요한가: 앞선 시도가 만들다 실패하면 컨테이너가 남는다. 그대로 두면
	// 다음 시도가 409 로 실패하고, 사람은 도커를 직접 만져야 한다. 우리 이름
	// 규칙(dbstudio-*)을 쓰는 것만 치우므로 남의 컨테이너를 건드리지 않는다.
	r.progress(id, "컨테이너를 만듭니다")
	if err := r.removeStale(ctx, plan.Container); err != nil {
		return err
	}

	res, err := r.docker.CreateContainer(ctx, plan.Container, plan.CreateRequest(id))
	if err != nil {
		return err
	}
	if err := r.setContainer(id, res.ID); err != nil {
		return err
	}

	// 설정 파일은 **시작하기 전에** 넣는다. 시작한 뒤에 넣으면 이미 읽고 지나간 뒤다.
	for path, body := range plan.Files {
		r.progress(id, "설정 파일을 넣습니다: "+path)
		if err := r.docker.PutFile(ctx, res.ID, path, body); err != nil {
			return err
		}
	}

	r.progress(id, "시작합니다")
	if err := r.docker.StartContainer(ctx, res.ID); err != nil {
		return err
	}

	// 건강해질 때까지 기다린다.
	//
	// 시작했다고 곧 쓸 수 있는 것이 아니다. MySQL 은 처음 뜰 때 데이터
	// 디렉터리를 만들며 수십 초를 쓴다. 그 사이에 커넥션으로 등록하면 접속
	// 확인이 실패하고, 사람은 설정을 잘못한 것으로 읽는다.
	r.progress(id, "뜨기를 기다립니다")
	return r.waitReady(ctx, id, res.ID, plan)
}

// waitReady는 건강해질 때까지(또는 죽을 때까지) 기다린다.
func (r *Runner) waitReady(ctx context.Context, id, containerID string, plan *Plan) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ins, err := r.docker.InspectContainer(ctx, containerID)
		if err != nil {
			return err
		}

		// 죽었으면 로그를 붙여 알린다. 이유 없이 "실패"만 남으면 사람이
		// 도커를 직접 뒤져야 한다.
		if ins.State != nil && !ins.State.Running && ins.State.Status != "created" {
			return fmt.Errorf("컨테이너가 뜨자마자 멈췄습니다 (종료 코드 %d)%s",
				ins.State.ExitCode, r.tailLogs(containerID))
		}

		health := ins.HealthStatus()
		if health == "healthy" || (health == "" && ins.State.Running) {
			// 헬스체크가 없는 이미지도 있다. 그때는 "돌고 있다"가 최선이다.
			port := ins.HostPort(strconv.Itoa(plan.Port) + "/tcp")
			return r.markRunning(id, port, health)
		}
		if health == "unhealthy" {
			// 아직 기다린다. 처음 뜰 때 몇 번 실패하는 것이 정상인 이미지가 있다.
			if time.Now().After(deadline) {
				return fmt.Errorf("뜨지 못했습니다 (헬스체크 실패)%s", r.tailLogs(containerID))
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("5분 안에 뜨지 못했습니다 (상태: %s)%s",
				orDash(health), r.tailLogs(containerID))
		}
		time.Sleep(time.Second)
	}
}

// removeStale은 같은 이름의 우리 컨테이너를 치운다.
func (r *Runner) removeStale(ctx context.Context, name string) error {
	ins, err := r.docker.InspectContainer(ctx, name)
	if err != nil {
		if docker.IsNotFound(err) {
			return nil
		}
		return err
	}
	// 우리 것이 아니면 건드리지 않는다. 이름이 부딪힌 것을 알려 주는 편이,
	// 남이 만든 DB 를 조용히 지우는 것보다 낫다.
	if ins.Config.Labels[LabelManagedBy] != "dbstudio" {
		return fmt.Errorf("%s 라는 컨테이너가 이미 있고 DB Studio 가 만든 것이 아닙니다. "+
			"다른 이름을 쓰거나 그 컨테이너를 먼저 치우세요", name)
	}
	return r.docker.RemoveContainer(ctx, ins.ID, true)
}

// tailLogs는 실패 메시지에 붙일 로그 몇 줄이다.
//
// 실패에 로그를 붙이는 이유: "뜨지 못했습니다"만으로는 비밀번호가 약한 것인지
// 메모리가 부족한 것인지 알 수 없고, 그것을 알려면 도커에 붙어야 한다 —
// 화면만으로 쓸 수 있어야 이 기능이 뜻이 있다.
func (r *Runner) tailLogs(containerID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logs, err := r.docker.Logs(ctx, containerID, docker.LogOptions{Tail: 12})
	if err != nil {
		return ""
	}
	defer logs.Close()
	out := ""
	for line := range logs.Lines() {
		if len(out) > 1200 {
			break
		}
		out += "\n" + line.Text
	}
	if out == "" {
		return ""
	}
	return "\n\n마지막 로그:" + out
}

// ---------- 상태 적기 ----------

func (r *Runner) progress(id, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.st.UpdateDBInstance(ctx, id, store.UpdateDBInstanceParams{Progress: &msg})
}

func (r *Runner) setContainer(id, containerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.st.UpdateDBInstance(ctx, id, store.UpdateDBInstanceParams{ContainerID: &containerID})
}

func (r *Runner) markRunning(id, port, health string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, empty := store.InstanceRunning, ""
	p := UpdateDBInstanceParamsWithPort(port)
	p.Status = &status
	p.Health = &health
	p.Error = &empty
	p.Progress = &empty
	return r.st.UpdateDBInstance(ctx, id, p)
}

// UpdateDBInstanceParamsWithPort는 포트를 담은 갱신 인자를 만든다.
//
// 따로 둔 이유: 포트를 못 읽었을 때(빈 문자열) 0 으로 덮어쓰면 안 된다.
// 앞서 잡아 둔 포트가 있는데 이번에 못 읽었다면, 그것은 우리가 모르는 것이지
// 포트가 없어진 것이 아니다.
func UpdateDBInstanceParamsWithPort(port string) store.UpdateDBInstanceParams {
	var p store.UpdateDBInstanceParams
	if port == "" {
		return p
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 {
		return p
	}
	p.HostPort = &n
	return p
}

func (r *Runner) fail(id string, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, msg, empty := store.InstanceFailed, cause.Error(), ""
	_ = r.st.UpdateDBInstance(ctx, id, store.UpdateDBInstanceParams{
		Status: &status, Error: &msg, Progress: &empty,
	})
}

// ---------- 실행/중단 ----------

// Start는 멈춰 있는 컨테이너를 시작한다.
func (r *Runner) Start(ctx context.Context, in *store.DBInstance) error {
	if in.ContainerID == "" {
		return errors.New("컨테이너가 없습니다. 다시 만들어야 합니다")
	}
	if err := r.docker.StartContainer(ctx, in.ContainerID); err != nil {
		return err
	}
	return r.Refresh(ctx, in)
}

// Stop은 컨테이너를 멈춘다.
func (r *Runner) Stop(ctx context.Context, in *store.DBInstance) error {
	if in.ContainerID == "" {
		return errors.New("컨테이너가 없습니다")
	}
	if err := r.docker.StopContainer(ctx, in.ContainerID, 20*time.Second); err != nil {
		return err
	}
	return r.Refresh(ctx, in)
}

// Restart는 멈추고 다시 시작한다.
func (r *Runner) Restart(ctx context.Context, in *store.DBInstance) error {
	if in.ContainerID == "" {
		return errors.New("컨테이너가 없습니다")
	}
	if err := r.docker.RestartContainer(ctx, in.ContainerID, 20*time.Second); err != nil {
		return err
	}
	return r.Refresh(ctx, in)
}

// Remove는 컨테이너를 지운다. 볼륨은 keepData 가 false 일 때만 지운다.
//
// 데이터를 지우는 것을 따로 묻는 이유: 컨테이너를 지우는 것과 데이터를 지우는
// 것은 되돌릴 수 있는 정도가 다르다. 컨테이너는 다시 만들면 되지만 볼륨은
// 지우면 끝이다.
func (r *Runner) Remove(ctx context.Context, in *store.DBInstance, keepData bool) error {
	if in.ContainerID != "" {
		if err := r.docker.RemoveContainer(ctx, in.ContainerID, true); err != nil {
			return err
		}
	}
	if !keepData && in.VolumeName != "" {
		if err := r.docker.RemoveVolume(ctx, in.VolumeName, false); err != nil {
			return err
		}
	}
	status, empty := store.InstanceRemoved, ""
	return r.st.UpdateDBInstance(ctx, in.ID, store.UpdateDBInstanceParams{
		Status: &status, ContainerID: &empty, Health: &empty, Progress: &empty,
	})
}

// Refresh는 도커의 지금 상태를 우리 줄에 적는다.
//
// 우리 status 는 캐시라, 화면을 열 때마다 이것으로 새로 고친다. 도커가
// 진짜를 들고 있으므로 어긋나면 도커가 맞다.
func (r *Runner) Refresh(ctx context.Context, in *store.DBInstance) error {
	if in.ContainerID == "" {
		return nil
	}
	ins, err := r.docker.InspectContainer(ctx, in.ContainerID)
	if err != nil {
		if docker.IsNotFound(err) {
			// 사람이 도커에서 직접 지운 것이다. 우리 줄을 맞춘다 — 없는
			// 컨테이너를 "도는 중"으로 보여 주면 시작·멈춤이 모두 실패한다.
			status, empty := store.InstanceRemoved, ""
			return r.st.UpdateDBInstance(ctx, in.ID, store.UpdateDBInstanceParams{
				Status: &status, ContainerID: &empty, Health: &empty,
			})
		}
		return err
	}

	status, health := store.InstanceStopped, ""
	if ins.State != nil && ins.State.Running {
		status = store.InstanceRunning
		// 건강 상태는 **돌고 있을 때만** 뜻이 있다.
		//
		// 도커는 멈춘 컨테이너의 마지막 헬스체크 결과를 그대로 들고 있고,
		// 멈추는 과정에서 그 결과는 거의 언제나 unhealthy 다. 그것을 그대로
		// 적으면 사람이 일부러 멈춘 DB 가 "이상 있음"으로 보인다 — 그러면
		// 필요한 것은 "시작"인데 화면은 "다시 시작"을 권한다.
		health = ins.HealthStatus()
		if health == "unhealthy" {
			status = store.InstanceUnhealthy
		}
	}
	p := UpdateDBInstanceParamsWithPort(ins.HostPort(strconv.Itoa(in.Port) + "/tcp"))
	p.Status = &status
	p.Health = &health
	return r.st.UpdateDBInstance(ctx, in.ID, p)
}

// Logs는 컨테이너 로그 스트림을 연다.
func (r *Runner) Logs(ctx context.Context, in *store.DBInstance, opt docker.LogOptions) (*docker.LogStream, error) {
	if in.ContainerID == "" {
		return nil, errors.New("컨테이너가 없어 로그를 읽을 수 없습니다")
	}
	return r.docker.Logs(ctx, in.ContainerID, opt)
}

// ---------- 유틸 ----------

// pullLine은 내려받기 진행을 한 줄로 만든다.
func pullLine(image string, p docker.PullProgress) string {
	if p.Detail.Total > 0 {
		pct := p.Detail.Current * 100 / p.Detail.Total
		return fmt.Sprintf("%s — %s %d%%", image, p.Status, pct)
	}
	if p.Status != "" {
		return image + " — " + p.Status
	}
	return image
}

func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
