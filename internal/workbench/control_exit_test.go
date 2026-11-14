package workbench

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestControllerExitLimitsDeriveFromStageBudgets(t *testing.T) {
	seen := map[string]bool{}
	var total time.Duration
	for _, stage := range exitStages {
		if seen[stage.name] || stage.budget <= 0 || stage.label == "" {
			t.Fatalf("invalid shutdown stage %+v", stage)
		}
		seen[stage.name] = true
		total += stage.budget
	}
	for _, name := range []string{exitStageHTTP, exitStagePhone, exitStageTerminals, exitStageJobs, exitStageMonitors, exitStageActivities, exitStageOperation, exitStageResources, exitStageRemote, exitStageRelease} {
		if !seen[name] {
			t.Fatalf("stage %s has no budget", name)
		}
	}
	for name, got := range map[string]time.Duration{
		"controllerExitBudget":      controllerExitBudget(),
		"controllerExitWaitLimit":   controllerExitWaitLimit(),
		"prepareUpdateLimit":        prepareUpdateLimit(),
		"prepareDesktopUpdateLimit": prepareDesktopUpdateLimit(),
	} {
		want := map[string]time.Duration{
			"controllerExitBudget":      total,
			"controllerExitWaitLimit":   total + exitWaitMargin,
			"prepareUpdateLimit":        controllerReadyTimeout + controllerRequestTimeout + handoffConflictWindow + total + exitWaitMargin,
			"prepareDesktopUpdateLimit": 2*(controllerReadyTimeout+controllerRequestTimeout+handoffConflictWindow+total+exitWaitMargin) + controllerRequestTimeout,
		}[name]
		if got != want {
			t.Fatalf("%s = %s, want %s", name, got, want)
		}
	}
	// The documented arithmetic in control_exit.go and the native shells.
	if controllerExitBudget() != 155*time.Second || prepareDesktopUpdateLimit() != 375*time.Second {
		t.Fatalf("documented limits changed: exit=%s prepare=%s", controllerExitBudget(), prepareDesktopUpdateLimit())
	}
	// A waiter must outlast a shutdown that stays within every stage budget.
	if controllerExitWaitLimit() <= controllerExitBudget() {
		t.Fatal("wait limit does not cover the summed stage budgets")
	}
}

// Cage.exe and the CI smoke wait for prepare-update with the same derived limit.
func TestNativeSmokeWaitsMatchPrepareUpdateLimit(t *testing.T) {
	want := int(prepareDesktopUpdateLimit() / time.Second)
	for path, pattern := range map[string]string{
		filepath.Join("..", "..", "desktop", "windows", "MainWindow.cs"): `PrepareUpdateWorstCaseSeconds = (\d+);`,
		filepath.Join("..", "..", "scripts", "windows-common.ps1"):       `\$script:PrepareUpdateWorstCaseSeconds = (\d+)`,
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		match := regexp.MustCompile(pattern).FindSubmatch(b)
		if match == nil {
			t.Fatalf("%s does not declare its prepare-update limit", path)
		}
		if got, _ := strconv.Atoi(string(match[1])); got != want {
			t.Fatalf("%s waits %d s for prepare-update, Go derives %d s", path, got, want)
		}
	}
}

func heldControllerLock(t *testing.T) (string, func()) {
	t.Helper()
	profile := t.TempDir()
	lock, err := openControllerLock(profile, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockController(lock); err != nil {
		t.Fatal(err)
	}
	// release may run on another goroutine; Once orders it with the cleanup.
	var once sync.Once
	release := func() { once.Do(func() { _ = unlockController(lock); _ = lock.Close() }) }
	t.Cleanup(release)
	return profile, release
}

var fastExitPolicy = exitWaitPolicy{poll: 5 * time.Millisecond, limit: 3 * time.Second, slack: 50 * time.Millisecond}

func testExitProgress(profile string, token string, budget time.Duration) *exitProgress {
	p := newExitProgress(profile)
	p.interval = 10 * time.Millisecond
	p.budgetOf = func(string) time.Duration { return budget }
	p.bind(os.Getpid(), token)
	return p
}

// A controller that keeps advancing through its stages is waited for, even
// when its whole shutdown is far longer than any single stage budget.
func TestWaitForExitFollowsAdvancingShutdown(t *testing.T) {
	profile, release := heldControllerLock(t)
	token := strings.Repeat("b", 64)
	p := testExitProgress(profile, token, 200*time.Millisecond)
	p.begin("test")
	go func() {
		for _, stage := range []string{exitStagePhone, exitStageTerminals, exitStageJobs, exitStageMonitors, exitStageActivities, exitStageOperation, exitStageResources, exitStageRemote, exitStageRelease} {
			time.Sleep(150 * time.Millisecond)
			p.stage(stage)
		}
		p.stop()
		release()
	}()
	policy := fastExitPolicy
	policy.slack = 0
	started := time.Now()
	if err := waitForControllerExit(context.Background(), profile, os.Getpid(), token, policy); err != nil {
		t.Fatalf("advancing shutdown reported as failure: %v", err)
	}
	if took := time.Since(started); took < 9*150*time.Millisecond {
		t.Fatalf("returned before the lock was released: %s", took)
	}
}

// The controller itself reports an overrun; the waiter fails long before its
// ceiling and names the stage, while the lock stays with the controller.
func TestWaitForExitReportsStalledStage(t *testing.T) {
	profile, _ := heldControllerLock(t)
	token := strings.Repeat("c", 64)
	p := testExitProgress(profile, token, 100*time.Millisecond)
	p.begin("test")
	p.stage(exitStageActivities)
	defer p.stop()
	started := time.Now()
	err := waitForControllerExit(context.Background(), profile, os.Getpid(), token, fastExitPolicy)
	if err == nil || !strings.Contains(err.Error(), "等待进行中的请求结束") {
		t.Fatalf("stalled stage not reported: %v", err)
	}
	if took := time.Since(started); took < 150*time.Millisecond || took > time.Second {
		t.Fatalf("stall reported after %s", took)
	}
	if controllerLockFree(profile) {
		t.Fatal("waiting released the controller lock")
	}
}

func writeTestExitRecord(t *testing.T, profile string, record controllerExitRecord) {
	t.Helper()
	p := newExitProgress(profile)
	p.write(record)
	if _, err := os.Stat(controllerExitRecordPath(profile)); err != nil {
		t.Fatal(err)
	}
}

// Without a progress report of its own (an older controller, a heartbeat that
// stopped within budget, or another controller's record) the waiter cannot
// declare a stall. It waits for the overall ceiling, never less.
func TestWaitForExitWithoutOwnOverrunWaitsForCeiling(t *testing.T) {
	token := strings.Repeat("d", 64)
	frozen := controllerExitRecord{PID: os.Getpid(), Owner: controllerExitOwner(token), Stage: exitStageJobs, Sequence: 3, Beat: 9, StageElapsedMS: 500, StageBudgetMS: 1000}
	foreign := frozen
	foreign.Owner = controllerExitOwner(strings.Repeat("e", 64))
	foreign.StageElapsedMS = 60000
	for name, record := range map[string]*controllerExitRecord{"legacy": nil, "frozen within budget": &frozen, "foreign overrun": &foreign} {
		t.Run(name, func(t *testing.T) {
			profile, release := heldControllerLock(t)
			if record != nil {
				writeTestExitRecord(t, profile, *record)
			}
			policy := fastExitPolicy
			policy.limit = 400 * time.Millisecond
			started := time.Now()
			err := waitForControllerExit(context.Background(), profile, os.Getpid(), token, policy)
			if took := time.Since(started); err == nil || !strings.Contains(err.Error(), "后台尚未退出") || took < policy.limit {
				t.Fatalf("gave up after %s: %v", took, err)
			}
			if controllerLockFree(profile) {
				t.Fatal("waiting released the controller lock")
			}
			go func() { time.Sleep(100 * time.Millisecond); release() }()
			if err := waitForControllerExit(context.Background(), profile, os.Getpid(), token, fastExitPolicy); err != nil {
				t.Fatalf("released lock not observed: %v", err)
			}
		})
	}
}

// An overrun the controller recorded is a stall even if it was its last write.
func TestWaitForExitTrustsRecordedOverrun(t *testing.T) {
	profile, _ := heldControllerLock(t)
	token := strings.Repeat("f", 64)
	writeTestExitRecord(t, profile, controllerExitRecord{PID: os.Getpid(), Owner: controllerExitOwner(token), Stage: exitStageJobs, Sequence: 3, Beat: 9, StageElapsedMS: 60000, StageBudgetMS: 15000})
	err := waitForControllerExit(context.Background(), profile, os.Getpid(), token, fastExitPolicy)
	if err == nil || !strings.Contains(err.Error(), "结束后台任务") || !strings.Contains(err.Error(), "60 秒") {
		t.Fatalf("recorded overrun ignored: %v", err)
	}
}

func TestWaitForExitHonoursCancellation(t *testing.T) {
	profile, _ := heldControllerLock(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := waitForControllerExit(ctx, profile, os.Getpid(), strings.Repeat("g", 64), fastExitPolicy)
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("cancelled wait continued: %v", err)
	}
	if controllerLockFree(profile) {
		t.Fatal("cancelled wait released the controller lock")
	}
}

func TestExitProgressStopsPublishingBeforeRelease(t *testing.T) {
	var none *exitProgress
	none.begin("nil")
	none.stage(exitStageJobs)
	none.stop()
	none.released(nil)

	profile := t.TempDir()
	token := strings.Repeat("h", 64)
	p := testExitProgress(profile, token, time.Minute)
	p.stage(exitStageJobs)
	p.stop()
	if _, err := os.Stat(controllerExitRecordPath(profile)); !os.IsNotExist(err) {
		t.Fatalf("progress published without a shutdown: %v", err)
	}
	p.begin("test")
	p.begin("second")
	record, ok := readControllerExitRecord(profile, os.Getpid(), controllerExitOwner(token))
	if !ok || record.Stage != exitStageHTTP || record.Reason != "test" || record.Sequence != 1 {
		t.Fatalf("begin record: %+v %v", record, ok)
	}
	p.stage(exitStageRelease)
	deadline := time.Now().Add(2 * time.Second)
	for {
		record, ok = readControllerExitRecord(profile, os.Getpid(), controllerExitOwner(token))
		if ok && record.Stage == exitStageRelease && record.Beat > 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat did not advance: %+v", record)
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.stop()
	p.stage(exitStageRemote)
	final, _ := os.ReadFile(controllerExitRecordPath(profile))
	time.Sleep(50 * time.Millisecond)
	after, _ := os.ReadFile(controllerExitRecordPath(profile))
	if string(final) != string(after) {
		t.Fatal("progress was published after the heartbeat stopped")
	}
}
