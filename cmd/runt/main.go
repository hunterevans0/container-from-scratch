package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"container-from-scratch/internal/runtime"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "hello":
		fmt.Println("hello from runt, a container runtime built from scratch")
	case "run":
		exit("runt", run(args))
	case "create":
		exit("runt", create(args))
	case "start":
		exit("runt", start(args))
	case "ps":
		exit("runt", ps(args))
	case "stop":
		exit("runt", stop(args))
	case "kill":
		exit("runt", kill(args))
	case "rm":
		exit("runt", rm(args))
	case "exec":
		exit("runt", execCommand(args))
	case "logs":
		exit("runt", logs(args))
	// Internal commands: runt runs itself with these.
	case "--init":
		exit("runt init", runtime.Init())
	case "--monitor":
		if runtime.Monitor() != nil {
			os.Exit(1)
		}
	case "--exec-init":
		exit("runt exec", runtime.ExecInit())
	default:
		usage()
		os.Exit(2)
	}
}

// exit ends runt after a command. If the error is the container's command
// exiting unsuccessfully, runt exits with the same code, like docker run;
// whatever went wrong has already been reported.
func exit(prefix string, err error) {
	if err == nil {
		return
	}
	if code, ok := runtime.ExitCode(err); ok {
		os.Exit(code)
	}
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, prefix+":", err)
	os.Exit(runtime.FailureCode(err))
}

// containerFlags defines the options run and create share.
func containerFlags(name string) (*flag.FlagSet, *runtime.Config) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	config := &runtime.Config{}
	flags.StringVar(&config.Rootfs, "rootfs", "", "path to an unpacked Linux root filesystem")
	flags.StringVar(&config.Name, "name", "", "a name to refer to the container by, besides its ID")
	flags.StringVar(&config.UsernsUser, "userns-user", "", "user whose /etc/subuid and /etc/subgid ranges to map the container onto (default: the invoking user)")
	flags.StringVar(&config.AppArmorProfile, "apparmor-profile", "", "loaded AppArmor profile to confine the command with (default: none)")
	flags.BoolVar(&config.DebugInit, "debug-init", false, "pause the container's init process until a debugger attaches, and print its host PID")
	resources := &config.Resources
	flags.Func("memory", "memory limit, such as 512m or 1g (the container may also swap this much)", func(s string) (err error) {
		resources.Memory, err = runtime.ParseBytes(s)
		return err
	})
	flags.Func("cpus", "number of CPUs the container may use, such as 1.5", func(s string) error {
		cpus, err := strconv.ParseFloat(s, 64)
		if err != nil || cpus <= 0 {
			return fmt.Errorf("invalid CPU count %q", s)
		}
		resources.CPUs = cpus
		return nil
	})
	flags.Func("pids-limit", "maximum number of processes and threads in the container", func(s string) error {
		limit, err := strconv.ParseInt(s, 10, 64)
		if err != nil || limit <= 0 {
			return fmt.Errorf("invalid process limit %q", s)
		}
		resources.PidsLimit = limit
		return nil
	})
	deviceFlag := func(name, usage string, limits *[]runtime.DeviceLimit, bytes bool) {
		flags.Func(name, usage+" (repeatable)", func(s string) error {
			limit, err := runtime.ParseDeviceLimit(s, bytes)
			if err != nil {
				return err
			}
			*limits = append(*limits, limit)
			return nil
		})
	}
	deviceFlag("device-read-bps", "limit reads from a disk, such as /dev/sda:1mb", &resources.DeviceReadBps, true)
	deviceFlag("device-write-bps", "limit writes to a disk, such as /dev/sda:1mb", &resources.DeviceWriteBps, true)
	deviceFlag("device-read-iops", "limit read operations per second on a disk, such as /dev/sda:100", &resources.DeviceReadIOPS, false)
	deviceFlag("device-write-iops", "limit write operations per second on a disk, such as /dev/sda:100", &resources.DeviceWriteIOPS, false)
	return flags, config
}

func parseContainerFlags(flags *flag.FlagSet, config *runtime.Config, args []string) error {
	if err := flags.Parse(args); err != nil {
		return err
	}
	config.Command = flags.Args()
	if config.Rootfs == "" {
		return errors.New("--rootfs is required")
	}
	if len(config.Command) == 0 {
		return errors.New("a command is required after the options")
	}
	return nil
}

func run(args []string) error {
	flags, config := containerFlags("run")
	flags.BoolVar(&config.Detach, "d", false, "run the container in the background and print its ID; see its output with runt logs")
	if err := parseContainerFlags(flags, config, args); err != nil {
		return err
	}
	if !config.Detach {
		return runtime.Run(*config)
	}
	id, err := runtime.Create(*config, true)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func create(args []string) error {
	flags, config := containerFlags("create")
	if err := parseContainerFlags(flags, config, args); err != nil {
		return err
	}
	id, err := runtime.Create(*config, false)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func start(args []string) error {
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	return eachContainer(flags.Args(), runtime.Start)
}

func stop(args []string) error {
	flags := flag.NewFlagSet("stop", flag.ContinueOnError)
	timeout := flags.Int("t", 10, "seconds to wait after SIGTERM before sending SIGKILL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return eachContainer(flags.Args(), func(ref string) error {
		return runtime.Stop(ref, time.Duration(*timeout)*time.Second)
	})
}

func kill(args []string) error {
	flags := flag.NewFlagSet("kill", flag.ContinueOnError)
	signal := flags.String("s", "KILL", "signal to send, by name (TERM, SIGUSR1) or number")
	if err := flags.Parse(args); err != nil {
		return err
	}
	sig, err := runtime.ParseSignal(*signal)
	if err != nil {
		return err
	}
	return eachContainer(flags.Args(), func(ref string) error {
		return runtime.Kill(ref, sig)
	})
}

func rm(args []string) error {
	flags := flag.NewFlagSet("rm", flag.ContinueOnError)
	force := flags.Bool("f", false, "kill the container first if it is running")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return eachContainer(flags.Args(), func(ref string) error {
		return runtime.Remove(ref, *force)
	})
}

// eachContainer runs action on every container named in refs, printing each
// one it succeeds for, as Docker does, and reporting the others.
func eachContainer(refs []string, action func(ref string) error) error {
	if len(refs) == 0 {
		return errors.New("at least one container name or ID is required")
	}
	failed := 0
	for _, ref := range refs {
		if err := action(ref); err != nil {
			fmt.Fprintln(os.Stderr, "runt:", err)
			failed++
			continue
		}
		fmt.Println(ref)
	}
	if failed > 0 {
		os.Exit(1)
	}
	return nil
}

func execCommand(args []string) error {
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		return errors.New("usage: runt exec CONTAINER COMMAND [ARG ...]")
	}
	command := args[1:]
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	return runtime.Exec(args[0], command)
}

func logs(args []string) error {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	var options runtime.LogOptions
	flags.BoolVar(&options.Follow, "f", false, "keep printing new output until the container stops")
	flags.BoolVar(&options.Timestamps, "t", false, "show the time of each line")
	flags.IntVar(&options.Tail, "tail", -1, "show only the last N lines (default: all)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: runt logs [-f] [-t] [--tail N] CONTAINER")
	}
	return runtime.Logs(flags.Arg(0), options, os.Stdout, os.Stderr)
}

func ps(args []string) error {
	flags := flag.NewFlagSet("ps", flag.ContinueOnError)
	all := flags.Bool("a", false, "show stopped containers too")
	if err := flags.Parse(args); err != nil {
		return err
	}
	states, err := runtime.ListContainers(*all)
	if err != nil {
		return err
	}
	table := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(table, "CONTAINER ID\tNAME\tCOMMAND\tCREATED\tSTATUS\tPID")
	now := time.Now()
	for _, state := range states {
		pid := ""
		if state.Pid != 0 {
			pid = strconv.Itoa(state.Pid)
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s ago\t%s\t%s\n", state.ID, state.Name, shortCommand(state.Command),
			humanDuration(now.Sub(state.Created)), statusText(state, now), pid)
	}
	return table.Flush()
}

func statusText(state *runtime.State, now time.Time) string {
	switch state.Status {
	case runtime.StatusCreated:
		return "Created"
	case runtime.StatusRunning:
		return "Up " + humanDuration(now.Sub(state.Started))
	}
	if state.ExitCode == nil || state.Finished.IsZero() {
		return "Exited (unknown)"
	}
	return fmt.Sprintf("Exited (%d) %s ago", *state.ExitCode, humanDuration(now.Sub(state.Finished)))
}

func shortCommand(command []string) string {
	text := strings.Join(command, " ")
	if len(text) > 30 {
		text = text[:29] + "…"
	}
	return strconv.Quote(text)
}

func humanDuration(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d < time.Second:
		return "less than a second"
	case d < time.Minute:
		return plural(int(d.Seconds()), "second")
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: runt COMMAND [OPTIONS]

Containers:
  run [-d] [OPTIONS] -- COMMAND [ARG ...]   create a container and run COMMAND in it
  create [OPTIONS] -- COMMAND [ARG ...]     set a container up without starting COMMAND
  start CONTAINER ...                       run the command of created containers
  ps [-a]                                   list running (with -a, all) containers
  stop [-t SECONDS] CONTAINER ...           SIGTERM, then SIGKILL after a timeout (default 10)
  kill [-s SIGNAL] CONTAINER ...            send a signal (default KILL)
  rm [-f] CONTAINER ...                     remove stopped (with -f, any) containers
  exec CONTAINER COMMAND [ARG ...]          run a command in a running container
  logs [-f] [-t] [--tail N] CONTAINER       show a container's output
  hello                                     print a greeting

OPTIONS for run and create:
  --rootfs PATH (required)  --name NAME  --userns-user NAME  --apparmor-profile NAME
  --memory SIZE  --cpus N  --pids-limit N  --device-{read,write}-{bps,iops} PATH:RATE
  --debug-init

CONTAINER is a name, an ID, or the start of an ID. Options go before other
arguments. Run "runt COMMAND -h" for details.
`)
}
