package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	goruntime "runtime"
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
	case "version":
		fmt.Println("runt 0.1.0")
		fmt.Println(goruntime.Version())
	case "run":
		exit("runt", run(args))
	case "create":
		exit("runt", create(args))
	case "start":
		exit("runt", start(args))
	case "attach":
		exit("runt", attach(args))
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
	case "spec":
		exit("runt", spec(args))
	case "pull":
		exit("runt", pull(args))
	case "images":
		exit("runt", images(args))
	case "rmi":
		exit("runt", rmi(args))
	case "login":
		exit("runt", login(args))
	case "logout":
		exit("runt", logout(args))
	case "network":
		exit("runt", networkCommand(args))
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
	flags.StringVar(&config.Rootfs, "rootfs", "", "path to an unpacked Linux root filesystem, shared read-only under the container's own writable layer")
	flags.StringVar(&config.Image, "image", "", "image to run: a name or ID in runt's image store (pulled if missing), or an OCI image layout directory with optional :TAG")
	flags.StringVar(&config.Bundle, "bundle", "", "OCI runtime bundle directory: config.json and the root filesystem it names")
	flags.BoolVar(&config.Interactive, "i", false, "connect runt's standard input to the command; in the background, keep its input open for runt attach")
	flags.BoolVar(&config.Tty, "t", false, "run the command on a pseudo-terminal of its own")
	flags.Func("entrypoint", "replace the image's entrypoint (\"\" for none), which also drops its default command", func(s string) error {
		config.Entrypoint = &s
		return nil
	})
	setEnv := func(s string) error {
		// -e NAME takes NAME's value from runt's own environment, as in
		// Docker, and is left out if it has none.
		if !strings.Contains(s, "=") {
			value, ok := os.LookupEnv(s)
			if !ok {
				return nil
			}
			s += "=" + value
		}
		if strings.HasPrefix(s, "=") {
			return fmt.Errorf("invalid environment variable %q", s)
		}
		config.SetEnv = append(config.SetEnv, s)
		return nil
	}
	flags.Func("e", "set an environment variable, NAME=VALUE (repeatable)", setEnv)
	flags.Func("env", "same as -e", setEnv)
	flags.StringVar(&config.Cwd, "w", "", "working directory inside the container")
	flags.StringVar(&config.Cwd, "workdir", "", "same as -w")
	flags.StringVar(&config.UserSpec, "u", "", "user to run as: USER[:GROUP], by name or ID")
	flags.StringVar(&config.UserSpec, "user", "", "same as -u")
	flags.StringVar(&config.Name, "name", "", "a name to refer to the container by, besides its ID")
	flags.StringVar(&config.Network, "network", "", "network to connect the container to: bridge (the default: an address of its own, and NAT to the outside), none (loopback only), or one made with runt network create, where containers find each other by name")
	publish := func(s string) error {
		mapping, err := runtime.ParsePortMapping(s)
		if err != nil {
			return err
		}
		config.Ports = append(config.Ports, mapping)
		return nil
	}
	flags.Func("p", "publish a container port on the host, [[HOSTIP:]HOSTPORT:]CONTAINERPORT[/udp], as in 8080:80 (repeatable)", publish)
	flags.Func("publish", "same as -p", publish)
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
	if err := flags.Parse(expandCombinedFlags(flags, args)); err != nil {
		return err
	}
	config.Command = flags.Args()
	sources := 0
	for _, source := range []string{config.Rootfs, config.Image, config.Bundle} {
		if source != "" {
			sources++
		}
	}
	if sources != 1 {
		return errors.New("give one of --rootfs, --image, or --bundle")
	}
	if config.Entrypoint != nil && config.Image == "" {
		return errors.New("--entrypoint only works with --image")
	}
	if config.Bundle != "" {
		if len(config.Command) > 0 {
			return errors.New("a bundle's command comes from its config.json; don't give one after the options")
		}
		return runtime.LoadBundle(config)
	}
	// An image has a default command; a root filesystem doesn't.
	if len(config.Command) == 0 && config.Image == "" {
		return errors.New("a command is required after the options")
	}
	if config.Image != "" {
		// Pull the image now if need be, as docker run does, so the
		// progress shows even for a container in the background.
		return runtime.EnsureImage(config.Image, os.Stderr)
	}
	return nil
}

// expandCombinedFlags splits combined single-letter flags, such as -it into
// -i -t, as docker accepts them; the flag package doesn't. It stops at the
// first argument that isn't a flag, so the container's command is left
// alone.
func expandCombinedFlags(flags *flag.FlagSet, args []string) []string {
	isBool := func(f *flag.Flag) bool {
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		return ok && b.IsBoolFlag()
	}
	var expanded []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-") {
			return append(expanded, args[i:]...)
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if f := flags.Lookup(name); f != nil {
			expanded = append(expanded, arg)
			// A flag's value is the next argument unless given with =.
			if !hasValue && !isBool(f) && i+1 < len(args) {
				i++
				expanded = append(expanded, args[i])
			}
			continue
		}
		var letters []string
		if !strings.HasPrefix(arg, "--") && !hasValue {
			for _, letter := range name {
				if f := flags.Lookup(string(letter)); f == nil || !isBool(f) {
					letters = nil
					break
				}
				letters = append(letters, "-"+string(letter))
			}
		}
		if letters == nil {
			// Unknown: leave it for the flag package to report.
			letters = []string{arg}
		}
		expanded = append(expanded, letters...)
	}
	return expanded
}

func run(args []string) error {
	flags, config := containerFlags("run")
	flags.BoolVar(&config.Detach, "d", false, "run the container in the background and print its ID; see its output with runt logs, or connect to it with runt attach")
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
	attach := flags.Bool("a", false, "attach to the container's output, and wait for it to exit")
	interactive := flags.Bool("i", false, "attach runt's input to the container too (implies -a)")
	if err := flags.Parse(expandCombinedFlags(flags, args)); err != nil {
		return err
	}
	if !*attach && !*interactive {
		return eachContainer(flags.Args(), runtime.Start)
	}
	if flags.NArg() != 1 {
		return errors.New("start -a and -i take a single container")
	}
	return runtime.Attach(flags.Arg(0), runtime.AttachOptions{Stdin: *interactive, SigProxy: true, Start: true})
}

func attach(args []string) error {
	flags := flag.NewFlagSet("attach", flag.ContinueOnError)
	noStdin := flags.Bool("no-stdin", false, "don't send runt's input to the container")
	sigProxy := flags.Bool("sig-proxy", true, "pass signals runt gets on to the container (without a terminal)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: runt attach [--no-stdin] [--sig-proxy=false] CONTAINER")
	}
	return runtime.Attach(flags.Arg(0), runtime.AttachOptions{Stdin: !*noStdin, SigProxy: *sigProxy})
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
	var options runtime.ExecOptions
	flags.BoolVar(&options.Interactive, "i", false, "connect runt's standard input to the command")
	flags.BoolVar(&options.Tty, "t", false, "run the command on a pseudo-terminal of its own")
	if err := flags.Parse(expandCombinedFlags(flags, args)); err != nil {
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		return errors.New("usage: runt exec [-i] [-t] CONTAINER COMMAND [ARG ...]")
	}
	command := args[1:]
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	return runtime.Exec(args[0], command, options)
}

func spec(args []string) error {
	flags := flag.NewFlagSet("spec", flag.ContinueOnError)
	bundle := flags.String("bundle", ".", "directory to write config.json in")
	rootfs := flags.String("rootfs", "rootfs", "root filesystem for config.json to name, relative to the bundle or absolute")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: runt spec [--bundle DIR] [--rootfs PATH]")
	}
	return runtime.WriteSpec(*bundle, *rootfs)
}

func pull(args []string) error {
	flags := flag.NewFlagSet("pull", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: runt pull IMAGE[:TAG|@DIGEST]")
	}
	return runtime.Pull(flags.Arg(0), os.Stdout)
}

func images(args []string) error {
	flags := flag.NewFlagSet("images", flag.ContinueOnError)
	quiet := flags.Bool("q", false, "show only image IDs")
	digests := flags.Bool("digests", false, "show the digest of images pulled by digest")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: runt images [-q] [--digests]")
	}
	list, err := runtime.ListImages()
	if err != nil {
		return err
	}
	if *quiet {
		seen := map[string]bool{}
		for _, image := range list {
			if id := shortID(image.ID); !seen[id] {
				seen[id] = true
				fmt.Println(id)
			}
		}
		return nil
	}
	table := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	header := "REPOSITORY\tTAG\tIMAGE ID\tCREATED\tSIZE"
	if *digests {
		header = "REPOSITORY\tTAG\tDIGEST\tIMAGE ID\tCREATED\tSIZE"
	}
	fmt.Fprintln(table, header)
	now := time.Now()
	for _, image := range list {
		created := "N/A"
		if !image.Created.IsZero() {
			created = humanDuration(now.Sub(image.Created)) + " ago"
		}
		digest := ""
		if *digests {
			digest = image.Digest + "\t"
			if image.Digest == "" {
				digest = "<none>\t"
			}
		}
		fmt.Fprintf(table, "%s\t%s\t%s%s\t%s\t%s\n", image.Repository, image.Tag, digest, shortID(image.ID), created, runtime.HumanSize(image.Size))
	}
	return table.Flush()
}

// shortID is the 12 hex digits Docker shows of an image ID.
func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

func rmi(args []string) error {
	flags := flag.NewFlagSet("rmi", flag.ContinueOnError)
	force := flags.Bool("f", false, "remove an image that has several names, or that containers were made from")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("usage: runt rmi [-f] IMAGE ...")
	}
	failed := 0
	for _, ref := range flags.Args() {
		if err := runtime.RemoveImage(ref, *force, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "runt:", err)
			failed++
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
	return nil
}

func login(args []string) error {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	user := flags.String("u", "", "user name")
	password := flags.String("p", "", "password (visible to other users in ps; prefer --password-stdin)")
	passwordStdin := flags.Bool("password-stdin", false, "read the password from standard input")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: runt login [-u USER] [-p PASSWORD | --password-stdin] [REGISTRY]")
	}
	if *passwordStdin {
		if *password != "" {
			return errors.New("give -p or --password-stdin, not both")
		}
		if *user == "" {
			return errors.New("--password-stdin needs -u")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		*password = strings.TrimRight(string(data), "\r\n")
	}
	if *user == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		var line string
		fmt.Scanln(&line)
		*user = strings.TrimSpace(line)
	}
	if *password == "" {
		var err error
		if *password, err = runtime.ReadPassword("Password: "); err != nil {
			return err
		}
	}
	path, err := runtime.Login(flags.Arg(0), *user, *password)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "WARNING! Your password is stored unencrypted in %s.\n", path)
	fmt.Println("Login Succeeded")
	return nil
}

func logout(args []string) error {
	flags := flag.NewFlagSet("logout", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: runt logout [REGISTRY]")
	}
	found, err := runtime.Logout(flags.Arg(0))
	if err != nil {
		return err
	}
	if !found {
		fmt.Println("Not logged in")
		return nil
	}
	fmt.Println("Removed login credentials")
	return nil
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
	fmt.Fprintln(table, "CONTAINER ID\tNAME\tCOMMAND\tCREATED\tSTATUS\tPORTS\tPID")
	now := time.Now()
	for _, state := range states {
		pid := ""
		if state.Pid != 0 {
			pid = strconv.Itoa(state.Pid)
		}
		var ports []string
		for _, port := range state.Ports {
			ports = append(ports, port.String())
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s ago\t%s\t%s\t%s\n", state.ID, state.Name, shortCommand(state.Command),
			humanDuration(now.Sub(state.Created)), statusText(state, now), strings.Join(ports, ", "), pid)
	}
	return table.Flush()
}

func networkCommand(args []string) error {
	const usage = "usage: runt network create [--subnet CIDR] NAME | ls | rm NAME ..."
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "create":
		flags := flag.NewFlagSet("network create", flag.ContinueOnError)
		subnet := flags.String("subnet", "", "the network's range of addresses, such as 10.90.0.0/24 (default: the first free /24 from 10.89.0.0/24 on)")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: runt network create [--subnet CIDR] NAME")
		}
		network, err := runtime.CreateNetwork(flags.Arg(0), *subnet)
		if err != nil {
			return err
		}
		fmt.Println(network.ID)
		return nil
	case "ls", "list":
		if len(args) != 1 {
			return errors.New("usage: runt network ls")
		}
		networks, err := runtime.ListNetworks()
		if err != nil {
			return err
		}
		table := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(table, "NETWORK ID\tNAME\tSUBNET\tBRIDGE")
		for _, network := range networks {
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", network.ID, network.Name, network.Subnet, network.Bridge)
		}
		return table.Flush()
	case "rm", "remove":
		if len(args) == 1 {
			return errors.New("usage: runt network rm NAME ...")
		}
		return eachContainer(args[1:], runtime.RemoveNetwork)
	}
	return errors.New(usage)
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
  run [-d] [-i] [-t] [OPTIONS] [-- COMMAND [ARG ...]]
                                              create a container and run COMMAND in it
  create [-i] [-t] [OPTIONS] [-- COMMAND [ARG ...]]
                                              set a container up without starting COMMAND
  start [-a] [-i] CONTAINER ...               run the command of created containers
  attach [--no-stdin] CONTAINER               connect to a running container's input and output
  ps [-a]                                     list running (with -a, all) containers
  stop [-t SECONDS] CONTAINER ...             SIGTERM, then SIGKILL after a timeout (default 10)
  kill [-s SIGNAL] CONTAINER ...              send a signal (default KILL)
  rm [-f] CONTAINER ...                       remove stopped (with -f, any) containers
  exec [-i] [-t] CONTAINER COMMAND [ARG ...]  run a command in a running container
  logs [-f] [-t] [--tail N] CONTAINER         show a container's output
  spec [--bundle DIR] [--rootfs PATH]         write a starting OCI config.json
  hello                                       print a greeting

Images:
  pull IMAGE                                  download an image from a registry
  images [-q] [--digests]                     list the images in runt's image store
  rmi [-f] IMAGE ...                          remove images from the store
  login [-u USER] [--password-stdin] [REGISTRY]
                                              store credentials for a registry (default Docker Hub)
  logout [REGISTRY]                           remove them

Networks:
  network create [--subnet CIDR] NAME         make a network whose containers find each other by name
  network ls                                  list networks
  network rm NAME ...                         remove networks

OPTIONS for run and create: one of
  --rootfs PATH  --image IMAGE  --bundle DIR (whose config.json gives the command)
and any of
  --name NAME  --network bridge|none|NAME  -p [[HOSTIP:]HOSTPORT:]PORT[/udp]
  --userns-user NAME  --apparmor-profile NAME
  -e NAME=VALUE  -w DIR  -u USER[:GROUP]  --entrypoint COMMAND
  --memory SIZE  --cpus N  --pids-limit N  --device-{read,write}-{bps,iops} PATH:RATE
  --debug-init

IMAGE is a name such as alpine:3.20 or ghcr.io/owner/name:tag, an image ID,
or an OCI image layout directory with an optional :TAG. With --image, the
command defaults to the image's own.

-i connects runt's input to the command, and -t gives the command a
pseudo-terminal; use both, as -it, for an interactive shell. In the
background (-dit), runt attach connects to it later; Ctrl+P, Ctrl+Q detaches.

CONTAINER is a name, an ID, or the start of an ID. Options go before other
arguments. Run "runt COMMAND -h" for details.
`)
}
