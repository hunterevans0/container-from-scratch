//go:build linux

package runtime

// Signal names, for runt kill -s. A signal is a small number the kernel
// delivers to a process to tell it something: SIGTERM (15) asks it to exit,
// SIGKILL (9) ends it outright, SIGINT (2) is what Ctrl+C sends, and so on.
// For how to read the Go here, see doc.go.

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
)

// signalNames maps each signal's name, without its SIG prefix, to its
// number. It is a map literal: a key, a colon, and its value, per line.
var signalNames = map[string]syscall.Signal{
	"ABRT":   syscall.SIGABRT,
	"ALRM":   syscall.SIGALRM,
	"BUS":    syscall.SIGBUS,
	"CHLD":   syscall.SIGCHLD,
	"CONT":   syscall.SIGCONT,
	"FPE":    syscall.SIGFPE,
	"HUP":    syscall.SIGHUP,
	"ILL":    syscall.SIGILL,
	"INT":    syscall.SIGINT,
	"IO":     syscall.SIGIO,
	"KILL":   syscall.SIGKILL,
	"PIPE":   syscall.SIGPIPE,
	"PROF":   syscall.SIGPROF,
	"PWR":    syscall.SIGPWR,
	"QUIT":   syscall.SIGQUIT,
	"SEGV":   syscall.SIGSEGV,
	"STOP":   syscall.SIGSTOP,
	"SYS":    syscall.SIGSYS,
	"TERM":   syscall.SIGTERM,
	"TRAP":   syscall.SIGTRAP,
	"TSTP":   syscall.SIGTSTP,
	"TTIN":   syscall.SIGTTIN,
	"TTOU":   syscall.SIGTTOU,
	"URG":    syscall.SIGURG,
	"USR1":   syscall.SIGUSR1,
	"USR2":   syscall.SIGUSR2,
	"VTALRM": syscall.SIGVTALRM,
	"WINCH":  syscall.SIGWINCH,
	"XCPU":   syscall.SIGXCPU,
	"XFSZ":   syscall.SIGXFSZ,
}

// ParseSignal accepts a signal name such as TERM or SIGTERM, in any case, or
// a number such as 15.
func ParseSignal(s string) (syscall.Signal, error) {
	// An if statement can start with a short declaration, scoped to the if:
	// number and err exist only inside it. Atoi succeeds only for a number.
	if number, err := strconv.Atoi(s); err == nil {
		// Linux's signals are numbered 1 to 64.
		if number <= 0 || number > 64 {
			return 0, fmt.Errorf("invalid signal number %d", number)
		}
		// syscall.Signal(number) converts the int to the Signal type.
		return syscall.Signal(number), nil
	}
	name := strings.TrimPrefix(strings.ToUpper(s), "SIG")
	// Looking up a key in a map gives its value and, with ", ok", whether
	// the key was there at all.
	if sig, ok := signalNames[name]; ok {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}
