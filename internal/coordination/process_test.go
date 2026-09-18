package coordination

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCoordinationHelperProcess(t *testing.T) {
	if os.Getenv("EDNEVNIK_COORDINATION_HELPER") != "1" {
		return
	}
	config, err := ConfigFromEnv()
	if err != nil {
		panic(err)
	}
	c, err := New(os.Getenv("EDNEVNIK_HELPER_ROOT"), Namespace{Profile: "family", Origin: "https://portal.example"}, config)
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	if raw := os.Getenv("EDNEVNIK_HELPER_TIMEOUT"); raw != "" {
		d, _ := time.ParseDuration(raw)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	lease, err := c.Acquire(ctx)
	if err != nil {
		fmt.Printf("error:%v\n", err)
		return
	}
	defer lease.Release()
	switch os.Getenv("EDNEVNIK_HELPER_MODE") {
	case "hold":
		fmt.Println("ready")
		_ = os.Stdout.Sync()
		time.Sleep(time.Hour)
	case "request":
		started := time.Now()
		err = lease.BeforeRequest(ctx)
		count, _, _, _ := lease.Status()
		fmt.Printf("request:%v:%d:%d\n", err, count, time.Since(started).Milliseconds())
	case "cooldown":
		if err = lease.BeforeRequest(ctx); err == nil {
			err = lease.RecordResponse(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"5"}}})
		}
		fmt.Printf("cooldown:%v\n", err)
	}
}

func helperCommand(root, mode string, timeout time.Duration, extra ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoordinationHelperProcess$")
	env := append(os.Environ(), "EDNEVNIK_COORDINATION_HELPER=1", "EDNEVNIK_HELPER_ROOT="+root, "EDNEVNIK_HELPER_MODE="+mode, "EDNEVNIK_REQUEST_INTERVAL=0s", "EDNEVNIK_COORDINATION_WAIT=2s")
	if timeout > 0 {
		env = append(env, "EDNEVNIK_HELPER_TIMEOUT="+timeout.String())
	}
	cmd.Env = append(env, extra...)
	return cmd
}

func TestProcessesShareBudgetAndRecoverAfterOwnerDeath(t *testing.T) {
	root := t.TempDir()
	holder := helperCommand(root, "hold", 0)
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, 6)
	if _, err := stdout.Read(ready); err != nil || !strings.Contains(string(ready), "ready") {
		t.Fatalf("holder readiness: %q %v", ready, err)
	}

	waiter := helperCommand(root, "request", 120*time.Millisecond)
	out, err := waiter.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "context deadline exceeded") {
		t.Fatalf("cancelled waiter: %v %q", err, out)
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()

	for i := 1; i <= 3; i++ {
		cmd := helperCommand(root, "request", 0)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("request %d: %v %q", i, err, out)
		}
		if !strings.Contains(string(out), ":"+strconv.Itoa(i)+":") {
			t.Fatalf("request %d did not observe shared count: %q", i, out)
		}
	}
}

func TestCooldownAndPacingSurviveProcessExit(t *testing.T) {
	root := t.TempDir()
	out, err := helperCommand(root, "cooldown", 0).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "cooldown:<nil>") {
		t.Fatalf("set cooldown: %v %q", err, out)
	}
	out, err = helperCommand(root, "request", 0).CombinedOutput()
	if err != nil || !strings.Contains(string(out), ErrServerCooldown.Error()) {
		t.Fatalf("later process ignored cooldown: %v %q", err, out)
	}
	time.Sleep(5100 * time.Millisecond)
	out, err = helperCommand(root, "request", 0).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "request:<nil>") {
		t.Fatalf("post-cooldown request: %v %q", err, out)
	}
	out, err = helperCommand(root, "request", 0, "EDNEVNIK_REQUEST_INTERVAL=5s").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.Fields(string(out))[0], ":")
	if len(parts) < 4 {
		t.Fatalf("request output %q", out)
	}
	elapsed, parseErr := strconv.Atoi(parts[len(parts)-1])
	if parseErr != nil || elapsed < 500 {
		t.Fatalf("later process did not preserve pacing: %q", out)
	}
}

func TestCancelledRequestIsNotCounted(t *testing.T) {
	config := DefaultConfig()
	config.RequestPace = 0
	c, err := New(t.TempDir(), Namespace{Profile: "family", Origin: "https://portal.example"}, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error=%v", err)
	}
	lease, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := lease.BeforeRequest(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("BeforeRequest error=%v", err)
	}
	count, _, _, err := lease.Status()
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}
