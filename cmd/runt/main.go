package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	"container-from-scratch/internal/runtime"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "hello":
		fmt.Println("hello from runt, a container runtime built from scratch")
	case "run":
		if err := run(os.Args[2:]); err != nil {
			// Exit with the container's exit code, like docker run. Init
			// has already reported any error of its own.
			if code, ok := runtime.ExitCode(err); ok {
				os.Exit(code)
			}
			fmt.Fprintln(os.Stderr, "runt:", err)
			os.Exit(1)
		}
	case "--init":
		if err := runtime.Init(); err != nil {
			if code, ok := runtime.ExitCode(err); ok {
				os.Exit(code)
			}
			fmt.Fprintln(os.Stderr, "runt init:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	rootfs := flags.String("rootfs", "", "path to an unpacked Linux root filesystem")
	usernsUser := flags.String("userns-user", "", "user whose /etc/subuid and /etc/subgid ranges to map the container onto (default: the invoking user)")
	apparmorProfile := flags.String("apparmor-profile", "", "loaded AppArmor profile to confine the command with (default: none)")
	debugInit := flags.Bool("debug-init", false, "pause the container's init process until a debugger attaches, and print its host PID")
	var resources runtime.Resources
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

	if err := flags.Parse(args); err != nil {
		return err
	}
	command := flags.Args()
	if *rootfs == "" {
		return errors.New("--rootfs is required")
	}
	if len(command) == 0 {
		return errors.New("a command is required after the run options")
	}
	return runtime.Run(runtime.Config{
		Rootfs:          *rootfs,
		Command:         command,
		UsernsUser:      *usernsUser,
		AppArmorProfile: *apparmorProfile,
		DebugInit:       *debugInit,
		Resources:       resources,
	})
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  runt hello")
	fmt.Fprintln(os.Stderr, "  runt run --rootfs PATH [--userns-user NAME] [--apparmor-profile NAME] [--debug-init]")
	fmt.Fprintln(os.Stderr, "           [--memory SIZE] [--cpus N] [--pids-limit N] [--device-{read,write}-{bps,iops} PATH:RATE]")
	fmt.Fprintln(os.Stderr, "           -- COMMAND [ARG ...]")
}
