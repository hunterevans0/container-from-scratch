package runtime

import (
	"errors"
	"fmt"
	"strings"
)

// Words and variables in a Dockerfile's arguments. Most instructions'
// arguments are processed much as a shell would: quotes group words and are
// removed, the escape character takes the next character as it is, and
// variables are replaced by their values:
//
//	$NAME or ${NAME}  the value, or nothing if NAME isn't set
//	${NAME:-WORD}     the value, or WORD if NAME is unset or empty
//	${NAME-WORD}      the value, or WORD if NAME is unset
//	${NAME:+WORD}     WORD if NAME is set and not empty, or else nothing
//	${NAME+WORD}      WORD if NAME is set, or else nothing
//	${NAME:?WORD}     the value, or fail with WORD if NAME is unset or empty
//	${NAME?WORD}      the value, or fail with WORD if NAME is unset
//
// Single quotes keep everything inside them as it is, variables included;
// double quotes keep spaces but still put in variables. RUN, CMD, and
// ENTRYPOINT are left alone, since the shell that runs them does this
// itself, with the container's own environment.
//
// This file has no build constraint, so its tests run anywhere. For how to
// read the Go here, see doc.go.

// wordLexer processes arguments, looking variables up with lookup, which
// reports whether one is set as well as its value.
type wordLexer struct {
	escape rune
	lookup func(name string) (string, bool)
}

// words splits text into words at spaces outside quotes, and processes
// each.
func (l wordLexer) words(text string) ([]string, error) {
	scanner := &wordScanner{lexer: l, text: []rune(text)}
	return scanner.scan(true, 0)
}

// word processes text as a single word, spaces and all, as WORKDIR and
// USER take theirs.
func (l wordLexer) word(text string) (string, error) {
	scanner := &wordScanner{lexer: l, text: []rune(text)}
	words, err := scanner.scan(false, 0)
	return strings.Join(words, ""), err
}

// keyValue is one NAME=VALUE of ENV, LABEL, or ARG. hasValue is false for an
// ARG NAME without one.
type keyValue struct {
	key, value string
	hasValue   bool
}

// keyValues reads the NAME=VALUE pairs of ENV, LABEL, or ARG. With legacy
// set, it also takes ENV's and LABEL's old form, NAME VALUE, whose value is
// the whole rest of the line. With bare set, as for ARG, a NAME may come
// without a value. Every value is processed with the variables as they were
// before the instruction, so ENV A=1 B=$A sets B to A's old value, as in
// Docker.
func (l wordLexer) keyValues(text string, legacy, bare bool) ([]keyValue, error) {
	first, rest := cutSpace(text)
	if legacy && !strings.Contains(first, "=") && rest != "" {
		value, err := l.word(rest)
		return []keyValue{{key: first, value: value, hasValue: true}}, err
	}
	words, err := l.words(text)
	if err != nil {
		return nil, err
	}
	var pairs []keyValue
	for _, word := range words {
		key, value, hasValue := strings.Cut(word, "=")
		if key == "" {
			return nil, fmt.Errorf("a name is missing in %q", word)
		}
		if !hasValue && !bare {
			return nil, fmt.Errorf("%q has no =; write NAME=VALUE", word)
		}
		pairs = append(pairs, keyValue{key: key, value: value, hasValue: hasValue})
	}
	return pairs, nil
}

// wordScanner works through text a character (a rune, which can be more
// than one byte) at a time, from pos.
type wordScanner struct {
	lexer wordLexer
	text  []rune
	pos   int
}

// scan reads words until the end of the text, or until stop, if it isn't 0,
// outside quotes, which it leaves for the caller. With split set, spaces and
// tabs outside quotes separate words; otherwise it is all one word. A word
// is still a word if it is empty, as "" is.
func (s *wordScanner) scan(split bool, stop rune) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	for s.pos < len(s.text) {
		ch := s.text[s.pos]
		if stop != 0 && ch == stop {
			break
		}
		switch {
		case split && (ch == ' ' || ch == '\t'):
			s.pos++
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
			continue
		case ch == s.lexer.escape:
			s.pos++
			if s.pos < len(s.text) {
				ch = s.text[s.pos]
				s.pos++
			}
			word.WriteRune(ch)
		case ch == '\'':
			end := s.pos + 1
			for end < len(s.text) && s.text[end] != '\'' {
				end++
			}
			if end == len(s.text) {
				return nil, errors.New("a single quote is never closed")
			}
			word.WriteString(string(s.text[s.pos+1 : end]))
			s.pos = end + 1
		case ch == '"':
			s.pos++
			if err := s.doubleQuoted(&word); err != nil {
				return nil, err
			}
		case ch == '$':
			value, err := s.variable()
			if err != nil {
				return nil, err
			}
			word.WriteString(value)
		default:
			word.WriteRune(ch)
			s.pos++
		}
		inWord = true
	}
	if inWord || !split {
		words = append(words, word.String())
	}
	return words, nil
}

// doubleQuoted reads the rest of a double-quoted string, up to and past its
// closing quote, into word. Inside, the escape character only escapes ",
// $, and itself.
func (s *wordScanner) doubleQuoted(word *strings.Builder) error {
	escapable := `"$` + string(s.lexer.escape)
	for s.pos < len(s.text) {
		ch := s.text[s.pos]
		switch {
		case ch == '"':
			s.pos++
			return nil
		case ch == s.lexer.escape && s.pos+1 < len(s.text) && strings.ContainsRune(escapable, s.text[s.pos+1]):
			word.WriteRune(s.text[s.pos+1])
			s.pos += 2
		case ch == '$':
			value, err := s.variable()
			if err != nil {
				return err
			}
			word.WriteString(value)
		default:
			word.WriteRune(ch)
			s.pos++
		}
	}
	return errors.New("a double quote is never closed")
}

// variable reads a variable reference starting at the $ at pos, and returns
// its value. A $ not followed by a name or { is just a $.
func (s *wordScanner) variable() (string, error) {
	s.pos++
	if s.pos >= len(s.text) {
		return "$", nil
	}
	if s.text[s.pos] != '{' {
		name := s.name()
		if name == "" {
			return "$", nil
		}
		value, _ := s.lexer.lookup(name)
		return value, nil
	}

	s.pos++
	name := s.name()
	if name == "" || s.pos >= len(s.text) {
		return "", errors.New("bad variable reference: ${ must be followed by a name and }")
	}
	value, set := s.lexer.lookup(name)
	// op is the modifier: "", or :- - :+ + :? ?.
	op := ""
	if s.text[s.pos] == ':' {
		op = ":"
		s.pos++
	}
	if s.pos < len(s.text) && strings.ContainsRune("-+?", s.text[s.pos]) {
		op += string(s.text[s.pos])
		s.pos++
	} else if op != "" || s.pos >= len(s.text) || s.text[s.pos] != '}' {
		return "", fmt.Errorf("bad variable reference ${%s...}: use ${%s}, or one of :- - :+ + :? ? and a word", name, name)
	}
	// The word after the modifier may itself have variables in it, even
	// ${...} ones, whose own } the call below reads; it stops at this one's
	// }.
	var word string
	if op != "" {
		words, err := s.scan(false, '}')
		if err != nil {
			return "", err
		}
		word = strings.Join(words, "")
	}
	if s.pos >= len(s.text) || s.text[s.pos] != '}' {
		return "", fmt.Errorf("bad variable reference ${%s: no closing }", name)
	}
	s.pos++

	empty := !set || value == ""
	switch op {
	case ":-":
		if empty {
			return word, nil
		}
	case "-":
		if !set {
			return word, nil
		}
	case ":+":
		if empty {
			return "", nil
		}
		return word, nil
	case "+":
		if !set {
			return "", nil
		}
		return word, nil
	case ":?", "?":
		if (op == ":?" && empty) || (op == "?" && !set) {
			if word == "" {
				word = "is not set"
			}
			return "", fmt.Errorf("%s: %s", name, word)
		}
	}
	return value, nil
}

// name reads a variable's name at pos: a letter or _, then letters, digits,
// and _. It returns "" if there isn't one.
func (s *wordScanner) name() string {
	start := s.pos
	for s.pos < len(s.text) {
		ch := s.text[s.pos]
		letter := ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
		if !letter && (s.pos == start || ch < '0' || ch > '9') {
			break
		}
		s.pos++
	}
	return string(s.text[start:s.pos])
}
