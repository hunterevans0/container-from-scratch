//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// iptablesRule is a rule for iptables: its table, its chain, and the rule
// itself.
type iptablesRule struct {
	table, chain string
	spec         []string
}

// runIptables runs iptables with args and returns its output. -w waits for
// another program changing the rules to finish, rather than failing.
func runIptables(args ...string) (string, error) {
	path, err := exec.LookPath("iptables")
	if err != nil {
		return "", errors.New("the bridge network needs iptables, for NAT: install it (apt install iptables), or use --network none")
	}
	output, err := exec.Command(path, append([]string{"-w"}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (r iptablesRule) run(command string) error {
	_, err := runIptables(append([]string{"-t", r.table, command, r.chain}, r.spec...)...)
	return err
}

// exists reports whether the rule is there.
func (r iptablesRule) exists() bool {
	return r.run("-C") == nil
}

// insert puts the rule first in its chain, ahead of any rules that would
// drop the packets, unless it is there already.
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
func ensureChain(table, chain string) error {
	if _, err := runIptables("-t", table, "-n", "-L", chain); err == nil {
		return nil
	}
	_, err := runIptables("-t", table, "-N", chain)
	return err
}
