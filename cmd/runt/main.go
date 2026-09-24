package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

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
			fmt.Fprintln(os.Stderr, "runt:", err)
			os.Exit(1)
		}
	case "--init":
		if err := runtime.Init(); err != nil {
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
	return runtime.Run(runtime.Config{Rootfs: *rootfs, Command: command})
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  runt hello")
	fmt.Fprintln(os.Stderr, "  runt run --rootfs PATH -- COMMAND [ARG ...]")
}
