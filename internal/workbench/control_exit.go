package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// A controller keeps its profile lock until every service it owns has stopped,
// so a second controller can never run beside it. Callers that hand a
// controller off therefore cannot shorten its shutdown; they only decide how
// long to wait for the lock. The controller publishes the stage it is in, and
// each stage has a budget. A waiter keeps waiting while the controller reports
// that its current stage is within budget, and fails only when the controller
// itself reports an overrun or the overall ceiling below is exhausted.

// Timeouts shared by the shutdown path and the launchers that wait for it.
const (
	// server.Shutdown waits this long for in-flight HTTP requests (server.go).
	httpShutdownGrace = 3 * time.Second
	// Client timeout of every launcher request to a controller (desktop.go).
	controllerRequestTimeout = 5 * time.Second
	// How long launchers wait for a starting or retiring controller to answer.
	controllerReadyTimeout = 12 * time.Second
	// Conflict retries before an idle handoff reports stop_required.
	handoffConflictWindow = 3 * time.Second
	// The desktop gateway locks the selected local view before it exits.
	localViewSuspendTimeout = 3 * time.Second
	// The gateway releases an idle controller it launched itself.
	releaseUnusedTimeout = 15 * time.Second
	// A controller launched for a cancelled connection is released within this.
	failedLaunchReleaseTimeout = 12 * time.Second
	// keepawake.holdProcess: 2 s for the helper to exit after stdin closes,
	// then kill and its 2 s WaitDelay.
	keepAwakeStopBound = 5 * time.Second
	// After server.Shutdown: server.Close and closing the HTTP error log.
	exitHTTPCloseBound = 2 * time.Second
	// Drain stages wait for work admitted before closing: terminals, jobs,
	// monitors, request handlers, holders of the operation lock, and guest
	// and proxy connections. Their bound is not provable from the waits
	// alone, because each depends on the code it waits for. All of that work
	// is cancelled when closing begins (beginClose cancels the lifetime and
	// every activity context derives from it), so it only has to observe
	// cancellation and clean up. The longest cleanup timeouts found on these
	// paths are 5 s (controllerRequestTimeout, controlContext and the
	// Windows proxy core's WaitForSingleObject); a drain stage gets three
	// times that. Exceeding it is reported as a stall, never shortened.
	exitDrainBudget = 15 * time.Second
	// Closing the listener, unlocking and closing the lock file.
	exitReleaseBound = 5 * time.Second
)

const (
	exitStageHTTP       = "http"
	exitStagePhone      = "phone"
	exitStageTerminals  = "terminals"
	exitStageJobs       = "jobs"
	exitStageMonitors   = "monitors"
	exitStageActivities = "activities"
	exitStageOperation  = "operation"
	exitStageResources  = "resources"
	exitStageRemote     = "remote"
	exitStageRelease    = "release"
)

// Stages in shutdown order. A desktop gateway or client-only controller goes
// from http straight to remote and release; it owns no workload.
var exitStages = []struct {
	name   string
	label  string
	budget time.Duration
}{
	{exitStageHTTP, "停止接收请求", httpShutdownGrace + exitHTTPCloseBound},
	{exitStagePhone, "断开手机连接", keepAwakeStopBound},
	{exitStageTerminals, "关闭终端", exitDrainBudget},
	{exitStageJobs, "结束后台任务", exitDrainBudget},
	{exitStageMonitors, "停止状态监测", exitDrainBudget},
	{exitStageActivities, "等待进行中的请求结束", exitDrainBudget},
	{exitStageOperation, "等待环境操作锁", exitDrainBudget},
	{exitStageResources, "释放环境连接与代理", exitDrainBudget},
	// Gateway: a selection change holding its lock (drain), the local view
	// suspension, the cancelled local connection (one controller request plus
	// its failed-launch release) and the release of an idle local controller.
	{exitStageRemote, "关闭工作环境连接", exitDrainBudget + localViewSuspendTimeout + controllerRequestTimeout + failedLaunchReleaseTimeout + releaseUnusedTimeout},
	{exitStageRelease, "释放控制锁", exitReleaseBound},
}

func exitStageInfo(name string) (string, time.Duration) {
	for _, stage := range exitStages {
		if stage.name == name {
			return stage.label, stage.budget
		}
	}
	return name, exitDrainBudget
}

// controllerExitBudget bounds a complete shutdown that stays within every
// stage budget: 5 + 5 + 6×15 + 50 + 5 = 155 s.
func controllerExitBudget() time.Duration {
	var total time.Duration
	for _, stage := range exitStages {
		total += stage.budget
	}
	return total
}

const (
	exitHeartbeatInterval = time.Second
	// A waiter tolerates this much beyond a stage budget: a heartbeat
	// interval of reporting delay plus scheduling on a loaded host.
	exitStageSlack = 3 * time.Second
	// Margin beyond the summed stage budgets before a waiter gives up.
	exitWaitMargin = 10 * time.Second
)

// controllerExitWaitLimit is the longest waitForExit waits: 165 s.
func controllerExitWaitLimit() time.Duration { return controllerExitBudget() + exitWaitMargin }

// prepareUpdateLimit is the worst case of PrepareUpdate without a stop: the
// ready wait (12 s plus one in-flight request), handoff conflict retries and
// the controller's exit: 12 + 5 + 3 + 165 = 185 s. Its unavailable branch
// (ready wait plus a 15 s guest probe) is shorter.
func prepareUpdateLimit() time.Duration {
	return controllerReadyTimeout + controllerRequestTimeout + handoffConflictWindow + controllerExitWaitLimit()
}

// prepareDesktopUpdateLimit is the worst case of prepare-update without a stop:
// the workload, one request probing the desktop gateway, then the gateway.
// Releasing the gateway is one request plus its exit (5 + 165 s), or, when
// the probe found it unavailable, another PrepareUpdate (185 s):
// 185 + 5 + 185 = 375 s. Native shells derive their timeout from this value.
func prepareDesktopUpdateLimit() time.Duration {
	return prepareUpdateLimit() + controllerRequestTimeout + prepareUpdateLimit()
}

func controllerExitRecordPath(profile string) string {
	return filepath.Join(profile, "control-exit.json")
}

// The owner digest binds a record to one controller instance without
// publishing its launcher token.
func controllerExitOwner(token string) string {
	digest := sha256.Sum256([]byte("controller exit\x00" + token))
	return hex.EncodeToString(digest[:])
}

type controllerExitRecord struct {
	PID            int    `json:"pid"`
	Owner          string `json:"owner"`
	Reason         string `json:"reason"`
	Stage          string `json:"stage"`
	Sequence       int    `json:"sequence"`
	Beat           uint64 `json:"beat"`
	StageElapsedMS int64  `json:"stage_elapsed_ms"`
	StageBudgetMS  int64  `json:"stage_budget_ms"`
	TotalElapsedMS int64  `json:"total_elapsed_ms"`
}

// exitProgress publishes one controller's shutdown. Every method is a no-op on
// a nil receiver and before begin, so unit tests and startup failures that
// never hand off write nothing.
type exitProgress struct {
	profile  string
	interval time.Duration
	budgetOf func(string) time.Duration
	mu       sync.Mutex
	pid      int
	owner    string
	begun    bool
	stopped  bool
	reason   string
	current  string
	sequence int
	beat     uint64
	started  time.Time
	entered  time.Time
	overrun  bool
	quit     chan struct{}
	done     chan struct{}
	// Heartbeat and stage writes race; the newest beat wins.
	writeMu sync.Mutex
	written uint64
}

func newExitProgress(profile string) *exitProgress {
	return &exitProgress{profile: profile, interval: exitHeartbeatInterval, budgetOf: func(stage string) time.Duration {
		_, budget := exitStageInfo(stage)
		return budget
	}}
}

func (p *exitProgress) bind(pid int, token string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.pid, p.owner = pid, controllerExitOwner(token)
	p.mu.Unlock()
}

// begin is idempotent. The first caller records why the controller is
// leaving and starts the heartbeat before any waiter is told to wait.
func (p *exitProgress) begin(reason string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.begun {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	p.begun, p.reason, p.started = true, reason, now
	p.enterLocked(exitStageHTTP, now)
	p.quit, p.done = make(chan struct{}), make(chan struct{})
	record := p.recordLocked(now)
	p.mu.Unlock()
	diagnostics.Log("info", "controller", "shutdown started: "+reason)
	p.write(record)
	go p.heartbeat()
}

func (p *exitProgress) enterLocked(stage string, now time.Time) {
	p.current, p.entered, p.overrun = stage, now, false
	p.sequence++
}

// stage marks the end of the current stage and the start of the next one.
func (p *exitProgress) stage(name string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.begun || p.stopped || p.current == name {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	previous, took := p.current, now.Sub(p.entered)
	p.enterLocked(name, now)
	record := p.recordLocked(now)
	total := now.Sub(p.started)
	p.mu.Unlock()
	diagnostics.Log("info", "controller", fmt.Sprintf("shutdown stage %s finished in %dms (total %dms); entering %s", previous, took.Milliseconds(), total.Milliseconds(), name))
	p.write(record)
}

func (p *exitProgress) recordLocked(now time.Time) controllerExitRecord {
	budget := p.budgetOf(p.current)
	p.beat++
	return controllerExitRecord{
		PID: p.pid, Owner: p.owner, Reason: p.reason, Stage: p.current, Sequence: p.sequence, Beat: p.beat,
		StageElapsedMS: now.Sub(p.entered).Milliseconds(), StageBudgetMS: budget.Milliseconds(),
		TotalElapsedMS: now.Sub(p.started).Milliseconds(),
	}
}

func (p *exitProgress) heartbeat() {
	defer close(p.done)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.quit:
			return
		case <-ticker.C:
		}
		p.mu.Lock()
		now := time.Now()
		record := p.recordLocked(now)
		label, _ := exitStageInfo(p.current)
		budget := p.budgetOf(p.current)
		report := !p.overrun && now.Sub(p.entered) > budget
		if report {
			p.overrun = true
		}
		p.mu.Unlock()
		if report {
			diagnostics.Log("warn", "controller", fmt.Sprintf("shutdown stage %s (%s) exceeded its %s budget", record.Stage, label, budget))
		}
		p.write(record)
	}
}

// stop ends the heartbeat before the lock is released. A record written after
// the release could describe a controller that no longer owns the profile.
func (p *exitProgress) stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.begun || p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	close(p.quit)
	p.mu.Unlock()
	<-p.done
}

// released logs the end of shutdown after the profile lock was released.
func (p *exitProgress) released(err error) {
	if p == nil {
		return
	}
	p.mu.Lock()
	begun, stage, took, total := p.begun, p.current, time.Since(p.entered), time.Since(p.started)
	p.mu.Unlock()
	if !begun {
		return
	}
	message := fmt.Sprintf("shutdown stage %s finished in %dms; controller lock released after %dms", stage, took.Milliseconds(), total.Milliseconds())
	if err != nil {
		diagnostics.Log("warn", "controller", message+" (explicit unlock failed: "+err.Error()+")")
		return
	}
	diagnostics.Log("info", "controller", message)
}

// The record holds no credential. Write failures are tolerated: the next
// heartbeat rewrites the complete state, and a waiter that sees no fresh
// record falls back to the overall ceiling instead of failing early.
func (p *exitProgress) write(record controllerExitRecord) {
	if record.Owner == "" {
		return
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if record.Beat <= p.written {
		return
	}
	p.written = record.Beat
	b, err := json.Marshal(record)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(p.profile, ".control-exit-*")
	if err != nil {
		return
	}
	temp := f.Name()
	_, err = f.Write(b)
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Rename(temp, controllerExitRecordPath(p.profile))
	}
	if err != nil {
		_ = os.Remove(temp)
	}
}

func readControllerExitRecord(profile string, pid int, owner string) (controllerExitRecord, bool) {
	b, err := os.ReadFile(controllerExitRecordPath(profile))
	if err != nil || len(b) > 4096 {
		return controllerExitRecord{}, false
	}
	var record controllerExitRecord
	if json.Unmarshal(b, &record) != nil || record.PID != pid || record.Owner != owner || record.Sequence < 1 {
		return controllerExitRecord{}, false
	}
	return record, true
}

type exitWaitPolicy struct {
	poll  time.Duration
	limit time.Duration
	slack time.Duration
}

var defaultExitWaitPolicy = exitWaitPolicy{poll: 100 * time.Millisecond, limit: controllerExitWaitLimit(), slack: exitStageSlack}

// waitForExit returns once the handed-off controller has released its profile
// lock. It never terminates the controller and never releases the lock itself.
func (c *controllerClient) waitForExit(ctx context.Context) error {
	return waitForControllerExit(ctx, c.profile, c.record.PID, c.record.Token, defaultExitWaitPolicy)
}

func waitForControllerExit(ctx context.Context, profile string, pid int, token string, policy exitWaitPolicy) error {
	f, err := openControllerLock(profile, os.O_RDWR)
	if err != nil {
		return errors.New("无法确认后台已退出，未继续更新。")
	}
	defer f.Close()
	owner := controllerExitOwner(token)
	start := time.Now()
	var last controllerExitRecord
	seen := false
	for {
		if lockController(f) == nil {
			_ = unlockController(f)
			return nil
		}
		// Only this controller's own record can declare a stall, and only by
		// reporting that its current stage exceeded the stage budget. A
		// missing record (an older controller) or a heartbeat that stopped
		// updating cannot, so the overall ceiling is all that remains.
		if record, ok := readControllerExitRecord(profile, pid, owner); ok {
			last, seen = record, true
			elapsed := time.Duration(record.StageElapsedMS) * time.Millisecond
			budget := time.Duration(record.StageBudgetMS) * time.Millisecond
			if elapsed > budget+policy.slack {
				label, _ := exitStageInfo(record.Stage)
				diagnostics.Log("warn", "controller", fmt.Sprintf("controller %d stalled in shutdown stage %s after %dms", pid, record.Stage, record.StageElapsedMS))
				return fmt.Errorf("后台在「%s」阶段已停留 %d 秒（上限 %d 秒），未强制结束进程。请稍后重试更新；仍失败时请导出诊断包反馈。", label, int(elapsed.Seconds()), int(budget.Seconds()))
			}
		}
		if waited := time.Since(start); waited > policy.limit {
			if seen {
				diagnostics.Log("warn", "controller", fmt.Sprintf("controller %d still in shutdown stage %s after %dms", pid, last.Stage, waited.Milliseconds()))
			}
			return errors.New("后台尚未退出，未强制结束进程。请稍后重试更新。")
		}
		select {
		case <-ctx.Done():
			return errors.New("等待后台退出已取消，未强制结束进程。")
		case <-time.After(policy.poll):
		}
	}
}
