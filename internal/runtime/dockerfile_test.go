package runtime

import (
	"slices"
	"strings"
	"testing"
)

// Tests for dockerfile.go, dockerfile_words.go, and dockerignore.go:
// reading Dockerfiles, processing their arguments, and .dockerignore
// patterns. For how Go tests work, see "Tests" in doc.go.

// TestParseDockerfile reads a Dockerfile with most of what the format
// allows: directives, comments, continuation lines, options, the exec
// form, and stages.
func TestParseDockerfile(t *testing.T) {
	text := "# syntax=docker/dockerfile:1\n" +
		"# escape=\\\n" +
		"ARG VERSION=3.20\n" +
		"\n" +
		"# A comment.\n" +
		"from alpine:${VERSION} AS Build\n" +
		"RUN apk add gcc \\\n" +
		"    # a comment inside an instruction\n" +
		"\n" +
		"    && mkdir /src\n" +
		"COPY --chown=1000:1000 --link hello.c /src/\n" +
		"FROM scratch\n" +
		"COPY --from=build /hello /hello\n" +
		`CMD ["/hello", "world"]` + "\r\n" +
		"ENTRYPOINT [not json\n"
	df, err := parseDockerfile(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(df.args) != 1 || df.args[0].args != "VERSION=3.20" {
		t.Errorf("global args = %+v", df.args)
	}
	if len(df.stages) != 2 || df.stages[0].name != "build" || df.stages[0].image != "alpine:${VERSION}" || df.stages[1].image != "scratch" {
		t.Fatalf("stages = %+v", df.stages)
	}
	build := df.stages[0].instructions
	if len(build) != 2 {
		t.Fatalf("first stage = %+v", build)
	}
	if run := build[0]; run.command != "RUN" || run.args != "apk add gcc     && mkdir /src" || run.line != 7 {
		t.Errorf("continued RUN = %+v", run)
	}
	if copy := build[1]; copy.flags["chown"] != "1000:1000" || copy.flags["link"] != "true" || copy.args != "hello.c /src/" {
		t.Errorf("COPY with options = %+v", copy)
	}
	final := df.stages[1].instructions
	if cmd := final[1]; !cmd.isExec || !slices.Equal(cmd.exec, []string{"/hello", "world"}) {
		t.Errorf("exec-form CMD = %+v", cmd)
	}
	if entrypoint := final[2]; entrypoint.isExec || entrypoint.args != "[not json" {
		t.Errorf("invalid JSON should be the shell form: %+v", entrypoint)
	}
}

// TestParseDockerfileEscape checks the escape directive: with `, a
// backslash is an ordinary character, as Windows paths want.
func TestParseDockerfileEscape(t *testing.T) {
	df, err := parseDockerfile(strings.NewReader("# escape=`\nFROM alpine\nRUN echo C:\\ `\n  done\n"))
	if err != nil {
		t.Fatal(err)
	}
	if df.escape != '`' || df.stages[0].instructions[0].args != `echo C:\   done` {
		t.Errorf("escape %q, RUN %q", df.escape, df.stages[0].instructions[0].args)
	}
}

// TestParseDockerfileErrors checks what a Dockerfile can get wrong.
func TestParseDockerfileErrors(t *testing.T) {
	for _, test := range []struct{ text, want string }{
		{"", "no instructions"},
		{"# just a comment\n", "no instructions"},
		{"RUN true\n", "before the first FROM"},
		{"ARG A=1\n", "no FROM"},
		{"FROM alpine\nBOGUS x\n", "unknown instruction BOGUS"},
		{"FROM alpine\nRUN --mount=type=cache,target=/x true\n", "BuildKit"},
		{"FROM alpine\nCOPY --frm=x a b\n", "no option --frm"},
		{"FROM alpine\nCOPY --from a b\n", "needs a value"},
		{"FROM alpine\nRUN\n", "needs arguments"},
		{"FROM alpine\nONBUILD RUN true\n", "ONBUILD"},
		{"FROM alpine\nRUN <<EOF\necho hi\nEOF\n", "here-documents"},
		{"FROM alpine AS a\nFROM alpine AS A\n", "already a stage"},
		{"FROM alpine AS 1st\n", "invalid stage name"},
		{"FROM alpine AS\n", "FROM takes"},
		{"# escape=x\nFROM alpine\n", "escape character"},
	} {
		if _, err := parseDockerfile(strings.NewReader(test.text)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("parseDockerfile(%q) = %v; want an error with %q", test.text, err, test.want)
		}
	}
}

// testLexer is a lexer with a few variables set: HOME, EMPTY (set, but
// empty), and SPACED, with a space in it.
func testLexer() wordLexer {
	vars := map[string]string{"HOME": "/root", "EMPTY": "", "SPACED": "a b"}
	return wordLexer{escape: '\\', lookup: func(name string) (string, bool) {
		value, ok := vars[name]
		return value, ok
	}}
}

// TestWords checks quotes, escapes, and variables.
func TestWords(t *testing.T) {
	for _, test := range []struct {
		text string
		want []string
	}{
		{"a b\tc", []string{"a", "b", "c"}},
		{`"a b" 'c d'`, []string{"a b", "c d"}},
		{`$HOME ${HOME}/x`, []string{"/root", "/root/x"}},
		{`'$HOME' "$HOME"`, []string{"$HOME", "/root"}},
		{`\$HOME a\ b`, []string{"$HOME", "a b"}},
		{`"a \"quoted\" \$HOME \x"`, []string{`a "quoted" $HOME \x`}},
		{`$SPACED`, []string{"a b"}},
		{`"" x`, []string{"", "x"}},
		{`$UNSET-x`, []string{"-x"}},
		{`${UNSET:-default} ${EMPTY:-default} ${EMPTY-default}`, []string{"default", "default", ""}},
		{`${HOME:+set} ${EMPTY:+set} ${EMPTY+set} ${UNSET+set}`, []string{"set", "", "set", ""}},
		{`${UNSET:-${HOME}/sub}`, []string{"/root/sub"}},
		{`${UNSET:-"a b"}`, []string{"a b"}},
		{`cost$ $1`, []string{"cost$", "$1"}},
	} {
		got, err := testLexer().words(test.text)
		if err != nil || !slices.Equal(got, test.want) {
			t.Errorf("words(%q) = %q, %v; want %q", test.text, got, err, test.want)
		}
	}
	for _, text := range []string{`"open`, `'open`, `${HOME`, `${}`, `${HOME:x}`, `${UNSET:?must be set}`, `${UNSET?}`} {
		if got, err := testLexer().words(text); err == nil {
			t.Errorf("words(%q) = %q; want an error", text, got)
		}
	}
	if got, err := testLexer().word(`"my dir"/$HOME  x`); err != nil || got != "my dir//root  x" {
		t.Errorf("word = %q, %v", got, err)
	}
}

// TestKeyValues checks reading ENV, LABEL, and ARG pairs, in both forms.
func TestKeyValues(t *testing.T) {
	lexer := testLexer()
	pairs, err := lexer.keyValues(`A=1 B="two words" C=$HOME D=`, true, false)
	want := []keyValue{{"A", "1", true}, {"B", "two words", true}, {"C", "/root", true}, {"D", "", true}}
	if err != nil || !slices.Equal(pairs, want) {
		t.Errorf("keyValues = %+v, %v", pairs, err)
	}
	pairs, err = lexer.keyValues(`GREETING hello there $HOME`, true, false)
	if err != nil || len(pairs) != 1 || pairs[0].value != "hello there /root" {
		t.Errorf("legacy form = %+v, %v", pairs, err)
	}
	pairs, err = lexer.keyValues(`NAME VERSION=1`, false, true)
	if err != nil || !slices.Equal(pairs, []keyValue{{"NAME", "", false}, {"VERSION", "1", true}}) {
		t.Errorf("ARG pairs = %+v, %v", pairs, err)
	}
	if _, err := lexer.keyValues(`A=1 B`, true, false); err == nil {
		t.Error("ENV accepted a name without a value")
	}
	if _, err := lexer.keyValues(`=1`, false, true); err == nil {
		t.Error("ARG accepted a value without a name")
	}
}

// TestDockerignore checks which paths .dockerignore patterns leave out.
func TestDockerignore(t *testing.T) {
	patterns, err := parseDockerignore(strings.NewReader(`
# comment
*.log
!keep.log
/build/
**/*.tmp
node_modules
docs/[a-c]*.md
!node_modules/keep/**
`))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"app.log":                   true,
		"keep.log":                  false,
		"sub/app.log":               false, // * doesn't match /
		"build":                     true,
		"build/out/x":               true, // inside an ignored directory
		"builder":                   false,
		"a.tmp":                     true,
		"deep/down/b.tmp":           true,
		"node_modules/lib/index.js": true,
		"node_modules/keep/x.js":    false,
		"docs/api.md":               true,
		"docs/zebra.md":             false,
		"main.go":                   false,
	} {
		if got := ignored(patterns, path); got != want {
			t.Errorf("ignored(%q) = %v; want %v", path, got, want)
		}
	}
	if !hasExceptions(patterns) {
		t.Error("hasExceptions = false")
	}
}
