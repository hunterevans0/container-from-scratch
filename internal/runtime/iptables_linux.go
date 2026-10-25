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

// exists reports whether the rule is there. iptables -C (check) succeeds if
// it is, and fails if it isn't.
func (r iptablesRule) exists() bool {
	return r.run("-C") == nil
}

// insert puts the rule first in its chain, ahead of any rules that would
// drop the packets, unless it is there already. Checking first makes
// setting up a network safe to repeat, which it is, for every container.
func (r iptablesRule) insert() error {
	if r.exists() {
		return nil
	}
	return r.run("-I")
}

// add puts the rule last in its chain.
func (r iptablesRule) add() error {
	return r.run("-A")
}

// remove deletes the rule, if it is there.
func (r iptablesRule) remove() error {
	if !r.exists() {
		return nil
	}
	return r.run("-D")
}

// ensureChain makes a chain of our own, unless it is there already.
// iptables -L lists a chain, and fails if there is no such chain; -N makes
// a new one.
func ensureChain(table, chain string) error {
	if _, err := runIptables("-t", table, "-n", "-L", chain); err == nil {
		return nil
	}
	_, err := runIptables("-t", table, "-N", chain)
	return err
}
