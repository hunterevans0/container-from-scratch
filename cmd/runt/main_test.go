package main

// Tests for main.go's option handling. A test function's name starts with
// Test and it takes a *testing.T, which it reports failures through; go
// test finds and runs them (see "Tests" in internal/runtime/doc.go).

import (
	"flag"
	"slices"
	"testing"
)

// TestExpandCombinedFlags checks that combined single-letter flags such as
// -it are split up, while flag values and the container's command are left
// alone.
//
// It makes a small set of options like run's, then checks each case: the
// arguments given, and what they should be expanded to.
func TestExpandCombinedFlags(t *testing.T) {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.Bool("d", false, "")
	flags.Bool("i", false, "")
	flags.Bool("t", false, "")
	flags.String("name", "", "")
	for _, test := range []struct{ args, want []string }{
		{[]string{"-it", "--name", "web", "--", "ls", "-la"}, []string{"-i", "-t", "--name", "web", "--", "ls", "-la"}},
		{[]string{"--name", "-dit", "-dit", "sh"}, []string{"--name", "-dit", "-d", "-i", "-t", "sh"}},
		{[]string{"--name=x", "-t", "web", "-it"}, []string{"--name=x", "-t", "web", "-it"}},
		// Unknown letters are left for the flag package to report.
		{[]string{"-ix"}, []string{"-ix"}},
	} {
		if got := expandCombinedFlags(flags, test.args); !slices.Equal(got, test.want) {
			t.Errorf("expandCombinedFlags(%q) = %q; want %q", test.args, got, test.want)
		}
	}
}
