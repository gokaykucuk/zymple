package execx_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gokaykucuk/zymple/internal/execx"
	"github.com/gokaykucuk/zymple/internal/execx/execxtest"
)

func TestOSRunsCommands(t *testing.T) {
	ctx := context.Background()
	out, err := execx.Output(ctx, execx.OS{}, "sh", "-c", "echo hello; echo problem >&2")
	if err != nil || out != "hello" {
		t.Fatalf("Output = %q, %v", out, err)
	}
	_, err = execx.Output(ctx, execx.OS{}, "sh", "-c", "echo broken >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "broken") || !strings.Contains(err.Error(), "sh -c") {
		t.Fatalf("error should carry the command and stderr, got %v", err)
	}
	var buf bytes.Buffer
	cmd := execx.Cmd{Name: "sh", Args: []string{"-c", "echo $ZYMPLE_TEST"}, Stdout: &buf, Env: []string{"ZYMPLE_TEST=from-env"}}
	if err := (execx.OS{}).Run(ctx, cmd); err != nil || strings.TrimSpace(buf.String()) != "from-env" {
		t.Fatalf("Env not applied: %q, %v", buf.String(), err)
	}
}

func TestStreamCombinesOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := execx.Stream(context.Background(), execx.OS{}, &buf, "sh", "-c", "echo out; echo err >&2"); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Fatalf("Stream output = %q", got)
	}
	if err := execx.Stream(context.Background(), execx.OS{}, &buf, "sh", "-c", "exit 1"); err == nil {
		t.Fatal("Stream should report a failing command")
	}
}

func TestWrapTruncatesArguments(t *testing.T) {
	cmd := execx.Cmd{Name: "docker", Args: []string{"run", "-d", "--name", "x", "-e", "SECRET=1"}}
	err := execx.Wrap(cmd, errors.New("boom"), "")
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaks later arguments: %v", err)
	}
	if got := cmd.String(); got != "docker run -d --name x -e SECRET=1" {
		t.Fatalf("String = %q", got)
	}
}

func TestFake(t *testing.T) {
	f := &execxtest.Fake{Handler: func(c execx.Cmd) (string, error) {
		if c.Args[0] == "fail" {
			return "", errors.New("nope")
		}
		return "ok\n", nil
	}}
	out, err := execx.Output(context.Background(), f, "tool", "run")
	if err != nil || out != "ok" {
		t.Fatalf("Output = %q, %v", out, err)
	}
	if _, err := execx.Output(context.Background(), f, "tool", "fail"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want error with stderr, got %v", err)
	}
	if f.Count("tool") != 2 || f.Find("fail") != "tool fail" || f.Find("absent") != "" {
		t.Fatalf("recording is wrong: %v", f.Calls())
	}
	f.Reset()
	if len(f.Calls()) != 0 {
		t.Fatal("Reset did not clear calls")
	}
	if err := (&execxtest.Fake{}).Run(context.Background(), execx.Cmd{Name: "x"}); err != nil {
		t.Fatal(err)
	}
}
