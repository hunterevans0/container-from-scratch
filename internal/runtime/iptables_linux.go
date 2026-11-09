//go:build linux

package runtime

// runt sets up NAT, published ports, and the walls between networks with
// iptables, the command-line tool for the kernel's packet filter (see the
// notes on iptables at portsChain in network_linux.go). Rather than talk to
// the kernel directly, it runs the iptables command, as Docker does. The
// functions here are the few ways it does so. For how to read the Go here,
// see doc.go.

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// iptablesRule is a rule for iptables: its table, its chain, and the rule
// itself, as words on iptables' command line. For example, table "nat",
// chain "POSTROUTING", and spec ["-s", "10.88.0.0/16", "-j", "MASQUERADE"]
// is the command iptables -t nat -A POSTROUTING -s 10.88.0.0/16 -j
// MASQUERADE, with -A (add) or whichever command is wanted.
type iptablesRule struct {
	table, chain string
	spec         []string
}

// runIptables runs iptables with args and returns its output. -w waits for
// another program changing the rules to finish, rather than failing.
func runIptables(args ...string) (string, error) {
	// LookPath finds the iptables program on the PATH, as a shell would.
	path, err := exec.LookPath("iptables")
	if err != nil {
		return "", errors.New("the bridge network needs iptables, for NAT: install it (apt install iptables), or use --network none")
	}
	// exec.Command prepares the program with its arguments, and
	// CombinedOutput runs it, waits for it, and returns what it printed.
	// An exit code other than 0 comes back as an error.
	output, err := exec.Command(path, append([]string{"-w"}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// run runs iptables with command, such as -A to add the rule or -D to
// delete it.
func (r iptablesRule) run(command string) error {
	_, err := runIptables(append([]string{"-t", r.table, command, r.chain}, r.spec...)...)
	return err
}

// remove deletes the rule, if it is there. iptables -C (check) succeeds if
// it is, and fails if it isn't.
func (r iptablesRule) remove() error {
	if r.run("-C") != nil {
		return nil
	}
	return r.run("-D")
}
