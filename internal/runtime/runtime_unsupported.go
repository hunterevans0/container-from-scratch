//go:build !linux

package runtime

import (
	"errors"
	"syscall"
	"time"
)

var errUnsupported = errors.New("containers require Linux; use WSL2 on Windows")

func Run(Config) error                           { return errUnsupported }
func Create(Config, bool) (string, error)        { return "", errUnsupported }
func Start(string) error                         { return errUnsupported }
func Stop(string, time.Duration) error           { return errUnsupported }
func Kill(string, syscall.Signal) error          { return errUnsupported }
func Remove(string, bool) error                  { return errUnsupported }
func ListContainers(bool) ([]*State, error)      { return nil, errUnsupported }
func Exec(string, []string) error                { return errUnsupported }
func ParseSignal(string) (syscall.Signal, error) { return 0, errUnsupported }
func Init() error                                { return errUnsupported }
func Monitor() error                             { return errUnsupported }
func ExecInit() error                            { return errUnsupported }
func ExitCode(error) (int, bool)                 { return 0, false }
func FailureCode(error) int                      { return 1 }
