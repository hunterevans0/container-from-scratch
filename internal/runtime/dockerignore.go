package runtime

import (
	"bufio"
	"io"
	"path"
	"regexp"
	"strings"
)

// .dockerignore, in the build context, lists files COPY and ADD can't see,
// as Docker reads it: one pattern per line, with # for comments.
//
//	*       any number of characters other than /
//	?       one character other than /
//	**      any number of directories, none included
//	[a-z]   one character in a range, as with filepath.Match
//	!       at the start, an exception: matching files are seen after all
//
// A pattern is relative to the context, and leading and trailing slashes
// don't matter. A path is left out if the last pattern it matches, or one
// of the directories it is in matches, isn't an exception. So
//
//	*.log
//	!keep.log
//
// leaves out every .log file in the context's top directory but keep.log,
// and node_modules leaves out that directory and everything in it.
//
// This file has no build constraint, so its tests run anywhere. For how to
// read the Go here, see doc.go.

// ignorePattern is one line of .dockerignore, made into a regular
// expression.
type ignorePattern struct {
	exception bool
	pattern   *regexp.Regexp
}

// parseDockerignore reads .dockerignore's patterns.
func parseDockerignore(r io.Reader) ([]ignorePattern, error) {
	var patterns []ignorePattern
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		exception := strings.HasPrefix(line, "!")
		line = strings.TrimSpace(strings.TrimPrefix(line, "!"))
		// path.Clean removes ./, doubled slashes, a trailing slash, and
		// resolves .. within the pattern.
		line = strings.TrimPrefix(path.Clean("/"+line), "/")
		if line == "" {
			continue
		}
		// Turn the pattern into a regular expression matching the same
		// paths, a character at a time. Regular expressions give special
		// meanings to characters a path may have, such as . and +, so
		// those are escaped with QuoteMeta.
		var expr strings.Builder
		expr.WriteString("^")
		for i := 0; i < len(line); i++ {
			ch := line[i]
			switch {
			case strings.HasPrefix(line[i:], "**/"):
				// Any number of directories, including none.
				expr.WriteString("(.*/)?")
				i += 2
			case strings.HasPrefix(line[i:], "**"):
				expr.WriteString(".*")
				i++
			case ch == '*':
				expr.WriteString("[^/]*")
			case ch == '?':
				expr.WriteString("[^/]")
			case ch == '[':
				// A range, written as regular expressions write them, up to
				// its ]; one that is never closed is taken as a [.
				end := strings.IndexByte(line[i+1:], ']')
				if end < 0 {
					expr.WriteString(`\[`)
					continue
				}
				class := line[i+1 : i+1+end]
				if strings.HasPrefix(class, "!") {
					class = "^" + class[1:]
				}
				expr.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
				i += end + 1
			case ch == '\\' && i+1 < len(line):
				i++
				expr.WriteString(regexp.QuoteMeta(line[i : i+1]))
			default:
				expr.WriteString(regexp.QuoteMeta(string(ch)))
			}
		}
		expr.WriteString("$")
		re, err := regexp.Compile(expr.String())
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, ignorePattern{exception: exception, pattern: re})
	}
	return patterns, scanner.Err()
}

// ignored reports whether patterns leave out rel, a slash-separated path
// relative to the context: whether the last pattern that matches it or a
// directory it is in isn't an exception.
func ignored(patterns []ignorePattern, rel string) bool {
	excluded := false
	for _, p := range patterns {
		// Whether the pattern matches rel, or one of the directories it is
		// in.
		for dir := rel; dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
			if p.pattern.MatchString(dir) {
				excluded = !p.exception
				break
			}
		}
	}
	return excluded
}

// hasExceptions reports whether any pattern is an exception, which could
// bring back a file inside a directory that is left out.
func hasExceptions(patterns []ignorePattern) bool {
	for _, p := range patterns {
		if p.exception {
			return true
		}
	}
	return false
}
