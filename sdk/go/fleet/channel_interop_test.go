package fleet_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"fleetplatform/sdk/go/fleet"
	"fleetplatform/sdk/go/internal/fleettest"
)

// Go sender against the Python SDK's receiver: sdk/python/examples/fake_robot.py
// accepts acked sends on channel "jobs" through on_acked. The two halves were
// written from the same convention (protocol/README.md); this is the check
// that they agree on the wire.
//
// It needs a Python that can import the SDK's dependencies. Set
// FLEET_TEST_PYTHON to the interpreter (for example a venv's bin/python);
// without it the test tries python3, and skips if that cannot import fleet.
func TestSendAckedToThePythonRobot(t *testing.T) {
	sdk, err := filepath.Abs(filepath.Join("..", "..", "python"))
	if err != nil {
		t.Fatal(err)
	}
	python := os.Getenv("FLEET_TEST_PYTHON")
	if python == "" {
		python = "python3"
	}
	env := append(os.Environ(), "PYTHONPATH="+sdk, "PYTHONUNBUFFERED=1")
	probe := exec.Command(python, "-c", "import fleet")
	probe.Env = env
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("%s cannot import the Python SDK (set FLEET_TEST_PYTHON): %v\n%s", python, err, out)
	}

	srv := fleettest.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	robot := exec.CommandContext(ctx, python, filepath.Join(sdk, "examples", "fake_robot.py"),
		"--url", srv.WSURL, "--name", "py-robot", "--no-wander",
		"--token-file", filepath.Join(t.TempDir(), "token.json"))
	robot.Env = append(env, "FLEET_ENROLL_KEY="+srv.EnrollKey)
	stdout, err := robot.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	robot.Stderr = robot.Stdout
	if err := robot.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		for sc := bufio.NewScanner(stdout); sc.Scan(); {
			lines <- sc.Text()
		}
	}()
	t.Cleanup(func() {
		cancel()
		robot.Wait()
	})
	var printed []string
	waitLine := func(what string, re *regexp.Regexp) []string {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("fake_robot.py exited before %s; output:\n%s", what, strings.Join(printed, "\n"))
				}
				printed = append(printed, line)
				if m := re.FindStringSubmatch(line); m != nil {
					return m
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s; output:\n%s", what, strings.Join(printed, "\n"))
			}
		}
	}
	robotID := waitLine("the robot to come online", regexp.MustCompile(`online as (r_\S+)`))[1]

	brain := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Service, Name: "dispatcher", EnrollKey: srv.EnrollKey})
	jobs := brain.Channel("jobs")
	start := time.Now()
	if err := jobs.SendAcked(context.Background(), robotID, map[string]any{"order": "o-1", "stops": 3}, 10*time.Second); err != nil {
		t.Fatalf("SendAcked to the Python robot: %v", err)
	}
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Errorf("acked after %v: the first copy was not the one acked", took)
	}
	job := waitLine("the robot to print the job", regexp.MustCompile(`job from (\S+): (.*)`))
	if job[1] != brain.ClientID() || job[2] != `{'order': 'o-1', 'stops': 3}` {
		t.Fatalf("the robot's application got %q from %q", job[2], job[1])
	}

	// The robot goes away: the next send learns it from the server.
	cancel()
	robot.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := jobs.SendAcked(context.Background(), robotID, "late", time.Second)
		if errors.Is(err, fleet.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SendAcked after the robot quit = %v, want ErrNotFound", err)
		}
	}
}
