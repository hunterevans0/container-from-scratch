//go:build linux

package runtime

// File locks, which runt uses wherever two runt processes might change the
// same thing at once: the image store, the networks' configuration and
// setup, and address reservations (see ipam_linux.go). For how to read the
// Go here, see doc.go.

import (
	"errors"
	"os"
	"sync"
	"syscall"
)

// flock takes or drops an advisory lock on file (see flock(2)), trying again
// if a signal interrupts it. The kernel drops the lock when the last
// descriptor for the open file is closed, including when its process dies.
//
// "Advisory" means the lock stops only other processes that also ask for it;
// nothing stops a process from reading or writing the file regardless. how
// is LOCK_SH (shared: any number of holders), LOCK_EX (exclusive: one
// holder, and no shared ones), or LOCK_UN (let go), and with LOCK_NB added,
// fail at once (EWOULDBLOCK) rather than wait. A system call interrupted by
// a signal fails with EINTR, and is simply made again.
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
//
// The returned function is a closure: it keeps hold of file and once from
// this call, after lockFile has returned. sync.Once runs the function given
// to its Do only the first time, so closing twice is harmless. Callers
// usually defer it.
func lockFile(path string, exclusive bool) (func(), error) {
	// O_RDWR|O_CREATE: open for reading and writing, creating the file if
	// it isn't there; | combines the flags. 0o600 is the new file's
	// permissions, in octal: read and write for the owner only.
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
