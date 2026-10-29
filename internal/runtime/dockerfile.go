package runtime

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// Reading a Dockerfile, for runt build (build_linux.go). A Dockerfile is a
// list of instructions, one per line: FROM starts a stage from an image,
// and the rest change that image, as RUN, COPY, and ADD do to its files,
// or only its configuration, as CMD, ENV, WORKDIR, and the like do.
//
//	# escape=\
//	ARG VERSION=3.20
//	FROM alpine:${VERSION} AS build
//	RUN apk add --no-cache gcc musl-dev \
//	    && mkdir /src
//	COPY hello.c /src/
//	RUN gcc -static -o /hello /src/hello.c
//
//	FROM scratch
//	COPY --from=build /hello /hello
//	CMD ["/hello"]
//
// Parsing turns the text into instructions, grouped by stage, joining
// lines that end in the escape character (\, unless an escape directive at
// the top says otherwise) and dropping comments. It doesn't put in
// variables' values, as ${VERSION} above: those depend on what the build
// has done so far, so the builder does it as it goes (see
// dockerfile_words.go).
//
// This file has no build constraint, so its tests run anywhere. For how to
// read the Go here, see doc.go.

// dockerfile is a parsed Dockerfile: the ARG instructions before its first
// FROM, which only FROM lines can use, and its stages.
type dockerfile struct {
	// escape is the escape character, \ or `.
	escape rune
	args   []instruction
	stages []*dockerStage
}

// dockerStage is a stage: a FROM, and the instructions after it up to the
// next one.
type dockerStage struct {
	// name is the stage's name from FROM IMAGE AS NAME, in lower case, or ""
	// if it has none; later stages can also refer to it by its number.
	name string
	from instruction
	// image is the image FROM names, as written: an image, a stage's name,
	// or scratch for none.
	image        string
	instructions []instruction
}

// instruction is one instruction, with continuation lines joined.
type instruction struct {
	// line is the line it starts on, for error messages.
	line int
	// command is the instruction's name, in upper case: FROM, RUN, ...
	command string
	// flags are its options, such as --from=build, by name (from). An
	// option given without a value, such as --link, is "true".
	flags map[string]string
	// args is the rest, after the options.
	args string
	// exec is args as a JSON array of strings, the "exec form" of RUN, CMD,
	// and others, as in CMD ["/hello", "world"]; isExec says whether it was
	// one.
	exec   []string
	isExec bool
	// original is the whole instruction as written, on one line, for
	// runt build's output and the image's history.
	original string
}

// instructionFlags lists the instructions runt build knows, and the options
// each one takes.
var instructionFlags = map[string][]string{
	"FROM":        {"platform"},
	"RUN":         nil,
	"CMD":         nil,
	"ENTRYPOINT":  nil,
	"SHELL":       nil,
	"ENV":         nil,
	"ARG":         nil,
	"LABEL":       nil,
	"MAINTAINER":  nil,
	"EXPOSE":      nil,
	"VOLUME":      nil,
	"USER":        nil,
	"WORKDIR":     nil,
	"STOPSIGNAL":  nil,
	"HEALTHCHECK": {"interval", "timeout", "start-period", "start-interval", "retries"},
	"COPY":        {"from", "chown", "chmod", "link"},
	"ADD":         {"chown", "chmod", "link", "checksum"},
	"ONBUILD":     nil,
}

// directivePattern matches a parser directive, a comment at the very top of
// the file such as "# escape=`": a name, =, and a value.
var directivePattern = regexp.MustCompile(`^#\s*([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)

// stageNamePattern is what a stage's name may be.
var stageNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)

// parseDockerfile reads a Dockerfile.
//
// It works in three passes: parser directives at the top, then the lines,
// joined into whole instructions (logical lines), each split into its
// command, options, and arguments; then the instructions are grouped into
// stages.
func parseDockerfile(r io.Reader) (*dockerfile, error) {
	var lines []string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		lines = append(lines, strings.TrimSuffix(scanner.Text(), "\r"))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Dockerfile: %w", err)
	}

	// Directives come first, and only first: the first line that isn't one
	// ends them, and any later one is just a comment. An unknown one is a
	// comment too, and so ends them. # syntax= names a BuildKit frontend
	// image, which runt doesn't use; it reads the file itself.
	df := &dockerfile{escape: '\\'}
	first := 0
	seen := map[string]bool{}
directives:
	for ; first < len(lines); first++ {
		match := directivePattern.FindStringSubmatch(lines[first])
		if match == nil {
			break
		}
		name := strings.ToLower(match[1])
		switch name {
		case "escape":
			if match[2] != `\` && match[2] != "`" {
				return nil, fmt.Errorf("line %d: the escape character must be \\ or `, not %q", first+1, match[2])
			}
			df.escape = rune(match[2][0])
		case "syntax", "check":
		default:
			// A break with a label ends the loop it names, not the
			// switch it is in.
			break directives
		}
		if seen[name] {
			return nil, fmt.Errorf("line %d: the %s directive is given twice", first+1, name)
		}
		seen[name] = true
	}

	// Join continuation lines into whole instructions. A line ending in the
	// escape character (and perhaps spaces) goes on to the next; comments
	// and blank lines in between are dropped, as Docker does.
	var instructions []instruction
	var text strings.Builder
	start := 0
	continuing := false
	escape := string(df.escape)
	for i := first; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !continuing {
			start = i + 1
		}
		if body := strings.TrimRight(line, " \t"); strings.HasSuffix(body, escape) {
			text.WriteString(strings.TrimSuffix(body, escape))
			continuing = true
			continue
		}
		text.WriteString(line)
		continuing = false
		inst, err := parseInstruction(text.String(), start)
		if err != nil {
			return nil, err
		}
		instructions = append(instructions, inst)
		text.Reset()
	}
	// The file ended in the middle of an instruction; Docker takes what is
	// there.
	if continuing && strings.TrimSpace(text.String()) != "" {
		inst, err := parseInstruction(text.String(), start)
		if err != nil {
			return nil, err
		}
		instructions = append(instructions, inst)
	}
	if len(instructions) == 0 {
		return nil, errors.New("the Dockerfile has no instructions")
	}

	// Group them into stages. Only ARG may come before the first FROM.
	names := map[string]bool{}
	for _, inst := range instructions {
		if inst.command == "FROM" {
			stage, err := parseFrom(inst)
			if err != nil {
				return nil, err
			}
			if stage.name != "" {
				if names[stage.name] {
					return nil, fmt.Errorf("line %d: there is already a stage called %q", inst.line, stage.name)
				}
				names[stage.name] = true
			}
			df.stages = append(df.stages, stage)
			continue
		}
		if len(df.stages) == 0 {
			if inst.command != "ARG" {
				return nil, fmt.Errorf("line %d: %s comes before the first FROM, where only ARG may", inst.line, inst.command)
			}
			df.args = append(df.args, inst)
			continue
		}
		stage := df.stages[len(df.stages)-1]
		stage.instructions = append(stage.instructions, inst)
	}
	if len(df.stages) == 0 {
		return nil, errors.New("the Dockerfile has no FROM instruction")
	}
	return df, nil
}

// parseInstruction splits a whole instruction into its command, options,
// and arguments.
func parseInstruction(text string, line int) (instruction, error) {
	text = strings.TrimSpace(text)
	keyword, rest := cutSpace(text)
	inst := instruction{line: line, command: strings.ToUpper(keyword), flags: map[string]string{}, original: text}
	// fail makes an error that says which line it is about.
	fail := func(format string, args ...any) (instruction, error) {
		return instruction{}, fmt.Errorf("line %d: "+format, append([]any{line}, args...)...)
	}
	allowed, known := instructionFlags[inst.command]
	if !known {
		return fail("unknown instruction %s", keyword)
	}
	if inst.command == "ONBUILD" {
		return fail("ONBUILD isn't supported")
	}
	// Options come first, each one word starting with --.
	for strings.HasPrefix(rest, "--") {
		var word string
		word, rest = cutSpace(rest)
		name, value, hasValue := strings.Cut(word[2:], "=")
		if !slices.Contains(allowed, name) {
			if inst.command == "RUN" {
				return fail("RUN --%s is BuildKit's, and runt build doesn't support it", name)
			}
			return fail("%s has no option --%s", inst.command, name)
		}
		if !hasValue {
			if name != "link" {
				return fail("--%s needs a value, as in --%s=VALUE", name, name)
			}
			value = "true"
		}
		inst.flags[name] = value
	}
	if rest == "" {
		return fail("%s needs arguments", inst.command)
	}
	inst.args = rest
	if (inst.command == "RUN" || inst.command == "COPY" || inst.command == "ADD") && strings.HasPrefix(rest, "<<") {
		return fail("here-documents (%s <<EOF) aren't supported", inst.command)
	}
	// The exec form is a JSON array of strings. Anything else, even if it
	// starts with [, is the shell form, as Docker takes it.
	if strings.HasPrefix(rest, "[") {
		var list []string
		if json.Unmarshal([]byte(rest), &list) == nil {
			inst.exec, inst.isExec = list, true
		}
	}
	return inst, nil
}

// parseFrom reads FROM [--platform=PLATFORM] IMAGE [AS NAME].
func parseFrom(inst instruction) (*dockerStage, error) {
	words := strings.Fields(inst.args)
	stage := &dockerStage{from: inst}
	switch {
	case len(words) == 1:
	case len(words) == 3 && strings.EqualFold(words[1], "AS"):
		stage.name = strings.ToLower(words[2])
		if !stageNamePattern.MatchString(stage.name) {
			return nil, fmt.Errorf("line %d: invalid stage name %q: use letters, digits, and _.- and start with a letter", inst.line, words[2])
		}
	default:
		return nil, fmt.Errorf("line %d: FROM takes an image, and optionally AS and a name", inst.line)
	}
	stage.image = words[0]
	return stage, nil
}

// cutSpace splits text at its first space or tab, returning the word before
// it and the rest, with the spaces in between left out.
func cutSpace(text string) (word, rest string) {
	end := strings.IndexAny(text, " \t")
	if end < 0 {
		return text, ""
	}
	return text[:end], strings.TrimLeft(text[end:], " \t")
}
