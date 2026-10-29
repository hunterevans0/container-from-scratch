// Command runt is a small container runtime, run as runt COMMAND [OPTIONS],
// as docker is. This file only reads the command line and prints results;
// the work is done by the runtime package, in internal/runtime (see its
// doc.go for an overview, and for how to read Go).
//
// package main is what makes a Go program, rather than a library: the
// program starts by calling main, below. os.Args holds the command line:
// os.Args[0] is the program's own name, os.Args[1] the command (run, ps,
// and so on), and the rest that command's options and arguments.
//
// Each command has a function of its own, which parses its options with the
// flag package: flags.StringVar(&variable, "name", default, help) defines
// an option --name (or -name), whose value is stored in variable when
// flags.Parse reads the arguments; BoolVar and IntVar likewise; flags.Func
// calls a function with each value instead. What is left after the options
// is flags.Args().
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

// main picks the function for the command named first on the command line,
// and passes it the rest. Each one returns an error, nil on success, which
// exit turns into runt's exit code.
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
	case "tag":
		exit("runt", tag(args))
	case "build":
		exit("runt", build(args))
	case "push":
		exit("runt", push(args))
	case "builder":
		exit("runt", builderCommand(args))
	case "login":
		exit("runt", login(args))
	case "logout":
		exit("runt", logout(args))
	case "network":
		exit("runt", networkCommand(args))
	case "volume":
		exit("runt", volumeCommand(args))
	// Internal commands: runt runs itself with these. Rather than ship
	// separate programs for the container's init, the background monitor,
	// and exec's helper, runt starts another copy of itself with one of
	// these as its command (see container_linux.go and exec_linux.go).
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
// whatever went wrong has already been reported. flag.ErrHelp is what
// parsing returns after printing the help for -h, which isn't a failure.
// os.Exit ends the program at once, with the given code.
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

// containerFlags defines the options run and create share. Each option
// stores its value straight into a field of config, through the field's
// address (&config.Rootfs), so parsing the command line fills in the
// container's configuration. ContinueOnError makes Parse return an error
// for a bad option, rather than exit.
func containerFlags(name string) (*flag.FlagSet, *runtime.Config) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	config := &runtime.Config{}
	flags.StringVar(&config.Rootfs, "rootfs", "", "path to an unpacked Linux root filesystem, shared read-only under the container's own writable layer")
	flags.StringVar(&config.Image, "image", "", "image to run: a name or ID in runt's image store (pulled if missing), or an OCI image layout directory with optional :TAG")
	flags.StringVar(&config.Bundle, "bundle", "", "OCI runtime bundle directory: config.json and the root filesystem it names")
	flags.BoolVar(&config.Interactive, "i", false, "connect runt's standard input to the command; in the background, keep its input open for runt attach")
	flags.BoolVar(&config.Tty, "t", false, "run the command on a pseudo-terminal of its own")
	// Entrypoint is a pointer, so "not given" (nil) differs from "given as
	// empty"; the function stores the address of its own copy of the value.
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
	// flags.Func registers an option whose value is handed to a function,
	// here publish, each time it is given, so -p can be repeated: each one
	// adds a mapping. -p and --publish share the function.
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
	// -v and --tmpfs work as -p does: each one adds a mount.
	volume := func(s string) error {
		mount, err := runtime.ParseVolume(s)
		if err != nil {
			return err
		}
		config.Mounts = append(config.Mounts, mount)
		return nil
	}
	flags.Func("v", "mount a host path or a volume in the container, SOURCE:DESTINATION[:ro], as in /srv/site:/site or pgdata:/var/lib/postgresql/data (repeatable)", volume)
	flags.Func("volume", "same as -v", volume)
	flags.Func("tmpfs", "mount an empty in-memory filesystem in the container, DESTINATION[:OPTIONS], as in /run:size=64m (repeatable)", func(s string) error {
		mount, err := runtime.ParseTmpfs(s)
		if err != nil {
			return err
		}
		config.Mounts = append(config.Mounts, mount)
		return nil
	})
	flags.StringVar(&config.UsernsUser, "userns-user", "", "user whose /etc/subuid and /etc/subgid ranges to map the container onto (default: the invoking user)")
	flags.StringVar(&config.AppArmorProfile, "apparmor-profile", "", "loaded AppArmor profile to confine the command with (default: none)")
	flags.BoolVar(&config.DebugInit, "debug-init", false, "pause the container's init process until a debugger attaches, and print its host PID")
	// A pointer to config's Resources, so the functions below can set its
	// fields more briefly. The memory function names its result (err), so
	// it can assign to it in the same statement as the limit.
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
	// deviceFlag defines one of the four disk limit options, which differ
	// only in their name, help, and which list they add to. limits is a
	// pointer to the list, so append's result can be stored back into it
	// (*limits = ...).
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

// parseContainerFlags reads run's or create's options and command into
// config, and checks they make sense together.
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
	// isBool reports whether f is an on/off option, which takes no value.
	// The flag package marks those with an IsBoolFlag method; the type
	// assertion to an interface written in place, interface{ IsBoolFlag()
	// bool }, asks whether the option's value has that method.
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
		// Not an option by its whole name: try it as single letters, each
		// of which must be an on/off option. Ranging over a string gives
		// each character (a rune) in turn.
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

// run is runt run: in the foreground, Run, which returns when the container
// exits; with -d, Create, which returns once it is running. *config passes
// the Config itself, a copy, rather than the pointer to it.
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

// create is runt create: set a container up, without starting its command.
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

// start is runt start. flags.Bool, unlike BoolVar, makes the variable
// itself and returns a pointer to it, so its value is read as *attach.
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

// attach is runt attach.
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

// stop is runt stop. The anonymous function stops one container; it can
// use timeout, from stop, since it is defined inside it. A time.Duration
// counts nanoseconds, so seconds are converted by multiplying by
// time.Second.
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

// kill is runt kill.
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

// rm is runt rm.
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
// one it succeeds for, as Docker does, and reporting the others. action is
// a function passed in by the caller, such as runtime.Start, or an
// anonymous function wrapping a call with more arguments.
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

// execCommand is runt exec. (exec alone isn't used as the name, so as not to
// be confused with the os/exec package.) The first argument after the
// options is the container, and the rest the command.
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

// spec is runt spec, which writes a starting config.json for a bundle.
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

// pull is runt pull.
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

// images is runt images, which lists the image store as a table.
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
	// With -q, each ID once, even for an image with several names: seen is
	// a set of the IDs printed so far.
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
	// A tabwriter lines up tab-separated columns, padding each with spaces
	// to its widest entry plus 3, once Flush is called.
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

// rmi is runt rmi, which removes each image named, reporting the ones it
// can't.
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

// tag is runt tag, which gives an image another name.
func tag(args []string) error {
	flags := flag.NewFlagSet("tag", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: runt tag SOURCE TARGET")
	}
	return runtime.Tag(flags.Arg(0), flags.Arg(1))
}

// build is runt build, which builds an image from a Dockerfile. -t and
// --build-arg can be given more than once, so each is a flags.Func adding
// to a list (or map) rather than a single value.
func build(args []string) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	options := runtime.BuildOptions{BuildArgs: map[string]string{}}
	addTag := func(s string) error {
		options.Tags = append(options.Tags, s)
		return nil
	}
	flags.Func("t", "name the image, NAME[:TAG], as in app:1.0 (repeatable)", addTag)
	flags.Func("tag", "same as -t", addTag)
	flags.StringVar(&options.Dockerfile, "f", "", "the Dockerfile (default: Dockerfile in the context; - for standard input)")
	flags.StringVar(&options.Dockerfile, "file", "", "same as -f")
	flags.Func("build-arg", "set an ARG, NAME=VALUE, or NAME to take its value from runt's environment (repeatable)", func(s string) error {
		name, value, hasValue := strings.Cut(s, "=")
		if !hasValue {
			// As with -e, a name alone takes runt's own value, and is left
			// out if there is none.
			var ok bool
			if value, ok = os.LookupEnv(name); !ok {
				return nil
			}
		}
		if name == "" {
			return fmt.Errorf("invalid build argument %q", s)
		}
		options.BuildArgs[name] = value
		return nil
	})
	flags.StringVar(&options.Target, "target", "", "build up to this stage, by name, and make it the image")
	flags.BoolVar(&options.NoCache, "no-cache", false, "run every step, rather than reuse what earlier builds made")
	flags.BoolVar(&options.Pull, "pull", false, "pull every image FROM names, even if runt's store has it")
	flags.StringVar(&options.Network, "network", "", "network for RUN steps: bridge (the default), none, or one made with runt network create")
	flags.StringVar(&options.UsernsUser, "userns-user", "", "user whose /etc/subuid and /etc/subgid ranges to map RUN steps onto (default: the invoking user)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: runt build [-t NAME[:TAG]] [-f DOCKERFILE] [OPTIONS] CONTEXT")
	}
	options.Context = flags.Arg(0)
	_, err := runtime.Build(options, os.Stdout)
	return err
}

// push is runt push.
func push(args []string) error {
	flags := flag.NewFlagSet("push", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: runt push NAME[:TAG]")
	}
	return runtime.Push(flags.Arg(0), os.Stdout)
}

// builderCommand runs runt builder prune, which empties runt build's cache.
func builderCommand(args []string) error {
	if len(args) != 1 || args[0] != "prune" {
		return errors.New("usage: runt builder prune")
	}
	entries, reclaimed, err := runtime.PruneBuildCache()
	if err != nil {
		return err
	}
	fmt.Printf("Deleted %d build cache entries\n", entries)
	fmt.Printf("Total reclaimed space: %s\n", runtime.HumanSize(reclaimed))
	return nil
}

// login is runt login. The password comes from -p, from standard input with
// --password-stdin, or else from a prompt, typed without being shown.
// *password = ... sets the variable flags.String made, through its pointer.
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

// logout is runt logout. flags.Arg(0) is "" if no registry was given,
// which means Docker Hub.
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

// logs is runt logs.
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

// ps is runt ps, which lists containers as a table, a row each, with its
// columns separated by tabs for the tabwriter to line up.
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

// networkCommand runs runt network and its subcommands, create, ls, and rm,
// which args[0] names. The work is in the runtime package (see
// internal/runtime/networks_linux.go); this only reads the options and
// prints the results.
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
		// A tabwriter lines up tab-separated columns, padding each to its
		// widest entry, once Flush is called.
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
		// eachContainer, made for rm and the like, works for networks too:
		// it calls RemoveNetwork for each name, and reports each one.
		return eachContainer(args[1:], runtime.RemoveNetwork)
	}
	return errors.New(usage)
}

// volumeCommand runs runt volume and its subcommands, create, ls, and rm,
// which args[0] names, as networkCommand does for networks (see
// internal/runtime/volumes_linux.go for the work).
func volumeCommand(args []string) error {
	const usage = "usage: runt volume create NAME | ls [-q] | rm NAME ..."
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "create":
		if len(args) != 2 {
			return errors.New("usage: runt volume create NAME")
		}
		volume, err := runtime.CreateVolume(args[1])
		if err != nil {
			return err
		}
		fmt.Println(volume.Name)
		return nil
	case "ls", "list":
		flags := flag.NewFlagSet("volume ls", flag.ContinueOnError)
		quiet := flags.Bool("q", false, "show only volume names")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("usage: runt volume ls [-q]")
		}
		volumes, err := runtime.ListVolumes()
		if err != nil {
			return err
		}
		if *quiet {
			for _, volume := range volumes {
				fmt.Println(volume.Name)
			}
			return nil
		}
		table := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(table, "VOLUME NAME\tCREATED\tMOUNTPOINT")
		now := time.Now()
		for _, volume := range volumes {
			fmt.Fprintf(table, "%s\t%s ago\t%s\n", volume.Name, humanDuration(now.Sub(volume.Created)), volume.Mountpoint)
		}
		return table.Flush()
	case "rm", "remove":
		if len(args) == 1 {
			return errors.New("usage: runt volume rm NAME ...")
		}
		return eachContainer(args[1:], runtime.RemoveVolume)
	}
	return errors.New(usage)
}

// statusText is a container's STATUS as runt ps shows it, as docker ps
// does: Created, Up 5 minutes, or Exited (0) 2 hours ago.
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

// shortCommand is a container's command for runt ps: at most 30 characters,
// in quotes. strconv.Quote adds the quotes, and escapes anything unprintable.
func shortCommand(command []string) string {
	text := strings.Join(command, " ")
	if len(text) > 30 {
		text = text[:29] + "…"
	}
	return strconv.Quote(text)
}

// humanDuration is a duration in the largest whole unit that fits, as in
// "3 minutes", for CREATED and STATUS. plural is a small helper for "1
// minute" but "2 minutes".
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

// usage prints the list of commands. The text is one raw string, between
// backquotes, which keeps its line breaks as written.
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
  build [-t NAME[:TAG]] [-f DOCKERFILE] [--build-arg NAME=VALUE] [--target STAGE]
        [--no-cache] [--pull] [--network NETWORK] CONTEXT
                                              build an image from a Dockerfile
  pull IMAGE                                  download an image from a registry
  push NAME[:TAG]                             upload an image to its registry
  tag SOURCE TARGET                           give an image another name
  images [-q] [--digests]                     list the images in runt's image store
  rmi [-f] IMAGE ...                          remove images from the store
  builder prune                               empty runt build's cache
  login [-u USER] [--password-stdin] [REGISTRY]
                                              store credentials for a registry (default Docker Hub)
  logout [REGISTRY]                           remove them

Networks:
  network create [--subnet CIDR] NAME         make a network whose containers find each other by name
  network ls                                  list networks
  network rm NAME ...                         remove networks

Volumes:
  volume create NAME                          make a volume, which outlives the containers using it
  volume ls [-q]                              list volumes
  volume rm NAME ...                          remove volumes no container uses

OPTIONS for run and create: one of
  --rootfs PATH  --image IMAGE  --bundle DIR (whose config.json gives the command)
and any of
  --name NAME  --network bridge|none|NAME  -p [[HOSTIP:]HOSTPORT:]PORT[/udp]
  -v HOSTPATH|VOLUME:PATH[:ro]  --tmpfs PATH[:size=SIZE,mode=MODE,...]
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
