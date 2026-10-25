//go:build !linux

package runtime

import (
	"errors"
	"io"
	"syscall"
	"time"
)

var errUnsupported = errors.New("containers require Linux; use WSL2 on Windows")

func Run(Config) error                               { return errUnsupported }
func Create(Config, bool) (string, error)            { return "", errUnsupported }
func Start(string) error                             { return errUnsupported }
func Stop(string, time.Duration) error               { return errUnsupported }
func Kill(string, syscall.Signal) error              { return errUnsupported }
func Remove(string, bool) error                      { return errUnsupported }
func ListContainers(bool) ([]*State, error)          { return nil, errUnsupported }
func Exec(string, []string, ExecOptions) error       { return errUnsupported }
func ParseSignal(string) (syscall.Signal, error)     { return 0, errUnsupported }
func Init() error                                    { return errUnsupported }
func Monitor() error                                 { return errUnsupported }
func ExecInit() error                                { return errUnsupported }
func ExitCode(error) (int, bool)                     { return 0, false }
func FailureCode(error) int                          { return 1 }
func Attach(string, AttachOptions) error             { return errUnsupported }
func Pull(string, io.Writer) error                   { return errUnsupported }
func EnsureImage(string, io.Writer) error            { return errUnsupported }
func ListImages() ([]ImageSummary, error)            { return nil, errUnsupported }
func RemoveImage(string, bool, io.Writer) error      { return errUnsupported }
func ReadPassword(string) (string, error)            { return "", errUnsupported }
func HumanSize(int64) string                         { return "" }
func CreateNetwork(string, string) (*Network, error) { return nil, errUnsupported }
func ListNetworks() ([]Network, error)               { return nil, errUnsupported }
func RemoveNetwork(string) error                     { return errUnsupported }

// ImageSummary describes an image in the store.
type ImageSummary struct {
	Repository, Tag, Digest, ID string
	Created                     time.Time
	Size                        int64
}
