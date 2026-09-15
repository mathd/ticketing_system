package cmdline

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestDispatchInvokesTheSelectedCommand(t *testing.T) {
	var received []string
	serverRuns := 0
	got := Dispatch([]string{"repair", "first", "second"}, Registry{
		"repair": WithArgs(func(args []string) error {
			received = append(received, args...)
			return errors.New("repair failed")
		}),
	}, func() error {
		serverRuns++
		return nil
	})

	if got.Name != "repair" || got.ExitCode != 1 || got.Err == nil {
		t.Fatalf("result = %+v, want the selected command's failure", got)
	}
	if !slices.Equal(received, []string{"first", "second"}) {
		t.Fatalf("command args = %v, want the tail after its name", received)
	}
	if serverRuns != 0 {
		t.Fatalf("server ran %d time(s) after a command was selected", serverRuns)
	}
}

func TestDispatchInvokesCommandsWithoutArguments(t *testing.T) {
	commandRuns := 0
	got := Dispatch([]string{"migrate"}, Registry{
		"migrate": WithoutArgs(func() error {
			commandRuns++
			return nil
		}),
	}, func() error {
		t.Fatal("server ran after a command was selected")
		return nil
	})

	if got.Name != "migrate" || got.ExitCode != 0 || got.Err != nil {
		t.Fatalf("result = %+v, want a successful migrate command", got)
	}
	if commandRuns != 1 {
		t.Fatalf("command ran %d time(s), want 1", commandRuns)
	}
}

func TestDispatchWithoutArgsRejectsTrailingArgumentsBeforeCallback(t *testing.T) {
	commandRuns := 0
	serverRuns := 0
	got := Dispatch([]string{"migrate", "trailing-arg"}, Registry{
		"migrate": WithoutArgs(func() error {
			commandRuns++
			return nil
		}),
	}, func() error {
		serverRuns++
		return nil
	})

	if got.Name != "migrate" || got.ExitCode == 0 || got.Err == nil {
		t.Fatalf("result = %+v, want nonzero exit and error for trailing arguments", got)
	}
	if commandRuns != 0 {
		t.Fatalf("command ran %d time(s) despite trailing arguments", commandRuns)
	}
	if serverRuns != 0 {
		t.Fatalf("server ran %d time(s) on trailing arguments", serverRuns)
	}
	if !strings.Contains(got.Err.Error(), "trailing") && !strings.Contains(got.Err.Error(), "argument") {
		t.Fatalf("err = %v, want usage error mentioning arguments", got.Err)
	}
}

func TestDispatchExitStatusRejectsTrailingArgumentsBeforeCallback(t *testing.T) {
	statusRuns := 0
	serverRuns := 0
	got := Dispatch([]string{"healthcheck", "trailing-arg"}, Registry{
		"healthcheck": ExitStatus(func() int {
			statusRuns++
			return 0
		}),
	}, func() error {
		serverRuns++
		return nil
	})

	if got.Name != "healthcheck" || got.ExitCode == 0 || got.Err == nil {
		t.Fatalf("result = %+v, want nonzero exit and error for trailing arguments", got)
	}
	if statusRuns != 0 {
		t.Fatalf("healthcheck ran %d time(s) despite trailing arguments", statusRuns)
	}
	if serverRuns != 0 {
		t.Fatalf("server ran %d time(s) on trailing arguments", serverRuns)
	}
}

func TestDispatchReturnsAStatusCommandCode(t *testing.T) {
	got := Dispatch([]string{"healthcheck"}, Registry{
		"healthcheck": ExitStatus(func() int { return 7 }),
	}, func() error {
		t.Fatal("server ran after healthcheck was selected")
		return nil
	})

	if got != (Result{Name: "healthcheck", ExitCode: 7}) {
		t.Fatalf("result = %+v, want healthcheck exit code 7", got)
	}
}

func TestDispatchStartsServerForMissingCommandOnly(t *testing.T) {
	serverErr := errors.New("server stopped")
	serverRuns := 0
	got := Dispatch(nil, Registry{
		"migrate": WithoutArgs(func() error { return nil }),
	}, func() error {
		serverRuns++
		return serverErr
	})
	if got.Name != "" || got.ExitCode != 1 || !errors.Is(got.Err, serverErr) {
		t.Fatalf("result = %+v, want the server failure", got)
	}
	if serverRuns != 1 {
		t.Fatalf("server ran %d time(s), want 1", serverRuns)
	}
}

func TestDispatchRejectsUnknownCommandWithoutStartingServer(t *testing.T) {
	serverRuns := 0
	got := Dispatch([]string{"not-a-command"}, Registry{
		"migrate": WithoutArgs(func() error { return nil }),
	}, func() error {
		serverRuns++
		return nil
	})

	if got.Name != "not-a-command" || got.ExitCode == 0 || got.Err == nil {
		t.Fatalf("result = %+v, want nonzero exit and error for unknown command", got)
	}
	if serverRuns != 0 {
		t.Fatalf("server ran %d time(s) for unknown command; want 0", serverRuns)
	}
	if !strings.Contains(got.Err.Error(), "unknown") {
		t.Fatalf("err = %v, want error mentioning unknown command", got.Err)
	}
}

func TestDispatchWithArgsPassesTailByteForByte(t *testing.T) {
	var captured []string
	serverRuns := 0
	tail := []string{"foo", "--bar=baz", "trailing with spaces", ""}
	got := Dispatch(append([]string{"custom"}, tail...), Registry{
		"custom": WithArgs(func(args []string) error {
			captured = append([]string(nil), args...)
			return nil
		}),
	}, func() error {
		serverRuns++
		return nil
	})

	if got.Name != "custom" || got.ExitCode != 0 || got.Err != nil {
		t.Fatalf("result = %+v, want successful dispatch", got)
	}
	if !slices.Equal(captured, tail) {
		t.Fatalf("captured tail = %#v, want %#v", captured, tail)
	}
	if serverRuns != 0 {
		t.Fatalf("server ran %d time(s)", serverRuns)
	}
}
