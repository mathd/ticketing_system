// Package cmdline dispatches service-binary subcommands without owning process exit.
package cmdline

// Command is one executable entry in a Registry.
type Command struct {
	withArgs    func([]string) error
	withoutArgs func() error
	exitStatus  func() int
}

// WithArgs registers a command that receives the arguments after its name.
func WithArgs(run func([]string) error) Command {
	return Command{withArgs: run}
}

// WithoutArgs registers a command whose existing CLI ignores trailing arguments.
func WithoutArgs(run func() error) Command {
	return Command{withoutArgs: run}
}

// ExitStatus registers a command such as a healthcheck that returns a process status.
func ExitStatus(run func() int) Command {
	return Command{exitStatus: run}
}

// Registry maps each CLI name to one command shape.
type Registry map[string]Command

// Result reports which command ran and the process status its caller should use.
// Name is empty when Dispatch selected the server.
type Result struct {
	Name     string
	ExitCode int
	Err      error
}

// Dispatch runs a named command or starts the server when args names no command.
// It never exits the process, so command selection can be exercised in tests.
func Dispatch(args []string, registry Registry, serve func() error) Result {
	if len(args) == 0 {
		return result("", serve())
	}

	name := args[0]
	command, ok := registry[name]
	if !ok {
		return result("", serve())
	}
	if command.withArgs != nil {
		return result(name, command.withArgs(args[1:]))
	}
	if command.withoutArgs != nil {
		return result(name, command.withoutArgs())
	}
	return Result{Name: name, ExitCode: command.exitStatus()}
}

func result(name string, err error) Result {
	exitCode := 0
	if err != nil {
		exitCode = 1
	}
	return Result{Name: name, ExitCode: exitCode, Err: err}
}
