package cmdline

import (
	"errors"
	"slices"
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
	got := Dispatch([]string{"migrate", "ignored-by-existing-cli"}, Registry{
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

func TestDispatchStartsServerForUnknownOrMissingCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "missing"},
		{name: "unknown", args: []string{"not-a-command"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverErr := errors.New("server stopped")
			serverRuns := 0
			got := Dispatch(tc.args, Registry{}, func() error {
				serverRuns++
				return serverErr
			})
			if got.Name != "" || got.ExitCode != 1 || !errors.Is(got.Err, serverErr) {
				t.Fatalf("result = %+v, want the server failure", got)
			}
			if serverRuns != 1 {
				t.Fatalf("server ran %d time(s), want 1", serverRuns)
			}
		})
	}
}
