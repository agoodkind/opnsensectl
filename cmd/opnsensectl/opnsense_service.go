package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// serviceConfigFlag is the one flag every long-running service takes. It has
// no default: a service starts only from the config file its command names.
const serviceConfigFlag = "config"

// serviceEnv is one environment variable a service requires at startup.
type serviceEnv struct {
	name    string
	purpose string
}

// serviceCommand describes a long-running service command: its words on the
// command line, its help text, and the environment it requires.
type serviceCommand struct {
	name        string
	description []string
	configDoc   string
	environment []serviceEnv
}

func (c serviceCommand) usage(out io.Writer) {
	fmt.Fprintf(out, "usage: opnsensectl %s --%s PATH\n", c.name, serviceConfigFlag)
	fmt.Fprintln(out, "")
	for _, line := range c.description {
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "  --%s PATH   required; %s\n", serviceConfigFlag, c.configDoc)
	fmt.Fprintln(out, "")
	if len(c.environment) == 0 {
		fmt.Fprintln(out, "Required environment: none.")
		return
	}
	fmt.Fprintln(out, "Required environment:")
	for _, env := range c.environment {
		fmt.Fprintf(out, "  %s   %s\n", env.name, env.purpose)
	}
}

// parseServiceArgs parses a service command's arguments and checks its
// environment. It returns the config path and ok=true when the service may
// start. Otherwise it returns the exit code: 0 after printing help on request,
// 2 when an argument or a required environment variable is missing or wrong.
func parseServiceArgs(cmd serviceCommand, args []string) (string, int, bool) {
	if len(args) == 1 && args[0] == "help" {
		cmd.usage(os.Stdout)
		return "", 0, false
	}
	flags := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String(serviceConfigFlag, "", "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			cmd.usage(os.Stdout)
			return "", 0, false
		}
		return "", cmd.fail(err.Error()), false
	}
	if flags.NArg() > 0 {
		return "", cmd.fail(fmt.Sprintf("unexpected arguments: %v", flags.Args())), false
	}
	path := strings.TrimSpace(*configPath)
	if path == "" {
		return "", cmd.fail(fmt.Sprintf("--%s PATH is required; there is no default config file", serviceConfigFlag)), false
	}
	for _, env := range cmd.environment {
		if strings.TrimSpace(os.Getenv(env.name)) == "" {
			return "", cmd.fail(fmt.Sprintf("environment variable %s is required: %s", env.name, env.purpose)), false
		}
	}
	return path, 0, true
}

// fail prints message and the command's help to stderr and returns the usage
// exit code.
func (c serviceCommand) fail(message string) int {
	fmt.Fprintf(os.Stderr, "opnsensectl %s: %s\n\n", c.name, message)
	c.usage(os.Stderr)
	return 2
}
