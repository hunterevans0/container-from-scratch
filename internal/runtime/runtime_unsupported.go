//go:build !linux

package runtime

// The build constraint above, !linux, is the opposite of the other files':
// this file is compiled only when building for anything but Linux, such as
// Windows. It defines every function cmd/runt calls, so that runt still
// builds there, but each one only returns errUnsupported, since containers
// need Linux's namespaces, cgroups, and the rest. On Windows, runt runs in
// WSL2 (see the README). For how to read the Go here, see doc.go.

import (
	"errors"
	"io"
	"syscall"
	"time"
)

var errUnsupported = errors.New("containers require Linux; use WSL2 on Windows")

// Each function's parameters are listed by type only, with no names, since
// none of them are used. The bodies are written on one line, which Go
// allows for short functions.
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
func Tag(string, string) error                       { return errUnsupported }
func Push(string, io.Writer) error                   { return errUnsupported }
func Build(BuildOptions, io.Writer) (string, error)  { return "", errUnsupported }
func PruneBuildCache() (int, int64, error)           { return 0, 0, errUnsupported }
func ReadPassword(string) (string, error)            { return "", errUnsupported }
func HumanSize(int64) string                         { return "" }
func CreateNetwork(string, string) (*Network, error) { return nil, errUnsupported }
func ListNetworks() ([]Network, error)               { return nil, errUnsupported }
func RemoveNetwork(string) error                     { return errUnsupported }
func CreateVolume(string) (*Volume, error)           { return nil, errUnsupported }
func ListVolumes() ([]Volume, error)                 { return nil, errUnsupported }
func RemoveVolume(string) error                      { return errUnsupported }

// ImageSummary describes an image in the store. On Linux it is defined in
// imagestore_linux.go; it is needed here too because ListImages returns it.
type ImageSummary struct {
	Repository, Tag, Digest, ID string
	Created                     time.Time
	Size                        int64
}
