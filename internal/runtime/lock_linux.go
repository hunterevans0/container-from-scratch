//go:build linux

package runtime

import (
	"errors"
	"os"
	"sync"
	"syscall"
)

// flock takes or drops an advisory lock on file (see flock(2)), trying again
// if a signal interrupts it. The kernel drops the lock when the last
// descriptor for the open file is closed, including when its process dies.
func flock(file *os.File, how int) error {
	for {
		err := syscall.Flock(int(file.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// lockFile takes a lock on the file at path, which it creates if need be,
// shared or exclusive, waiting for it if need be. It returns a function that
// releases it, which may be called more than once.
func lockFile(path string, exclusive bool) (func(), error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := flock(file, how); err != nil {
		file.Close()
		return nil, err
	}
	var once sync.Once
	// Closing the file releases the lock.
	return func() { once.Do(func() { file.Close() }) }, nil
}
