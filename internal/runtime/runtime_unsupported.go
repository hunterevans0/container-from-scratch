//go:build !linux

package runtime

import "errors"

func Run(Config) error {
	return errors.New("container execution requires Linux; use WSL2 on Windows")
}

func Init() error {
	return errors.New("container initialization requires Linux; use WSL2 on Windows")
}
