package main

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Pos represents a byte position in the original input text from which
// this template was parsed.
type Pos int

// item represents a token or text string returned from the scanner.
type item struct {
	typ  itemType // The type of this item.
	pos  Pos      // The starting position, in bytes, of this item in the input string.
	val  string   // The value of this item.
	line int      // The line number at the start of this item.
}

func (i item) String() string {
	switch {
	case i.typ == itemEOF:
		return "EOF"
	case i.typ == itemError:
		return i.val
	case i.typ > itemKeyword:
		return fmt.Sprintf("<%s>", i.val)
	case len(i.val) > 10:
		return fmt.Sprintf("%.10q...", i.val)
	}
	return fmt.Sprintf("%q", i.val)
}

// itemType identifies the type of lex items.
type itemType int

const (
	itemError itemType = iota // error occurred;

	itemEOF
	itemIdentifier
	itemString
	itemNumber
	itemKeyword
	itemComma
	itemStar
	itemSelect
	itemFrom
	itemSemicolon
)

var key = map[string]itemType{
	"*":      itemStar,
	"select": itemSelect,
	"from":   itemFrom,
	";":      itemSemicolon,
	",":      itemComma,
}

const eof = -1

const (
	spaceChars = " \t\r\n" // These are the space characters defined by Go itself.

)

// stateFn represents the state of the scanner as a function that returns the next state.
type stateFn func(*lexer) stateFn

// lexer holds the state of the scanner.
type lexer struct {
	name      string    // the name of the input; used only for error reports
	input     string    // the string being scanned
	pos       Pos       // current position in the input
	start     Pos       // start position of this item
	width     Pos       // width of last rune read from input
	items     chan item // channel of scanned items
	line      int       // 1+number of newlines seen
	startLine int       // start line of this item
}

// next returns the next rune in the input.
func (l *lexer) next() rune {
	if int(l.pos) >= len(l.input) {
		l.width = 0
		return eof
	}
	r, w := utf8.DecodeRuneInString(l.input[l.pos:])
	l.width = Pos(w)
	l.pos += l.width
	if r == '\n' {
		l.line++
	}
	return r
}

// peek returns but does not consume the next rune in the input.
func (l *lexer) peek() rune {
	r := l.next()
	l.backup()
	return r
}

// backup steps back one rune. Can only be called once per call of next.
func (l *lexer) backup() {
	l.pos -= l.width
	// Correct newline count.
	if l.width == 1 && l.input[l.pos] == '\n' {
		l.line--
	}
}

// emit passes an item back to the client.
func (l *lexer) emit(t itemType) {
	l.items <- item{t, l.start, l.input[l.start:l.pos], l.startLine}
	l.start = l.pos
	l.startLine = l.line
}

// ignore skips over the pending input before this point.
func (l *lexer) ignore() {
	l.line += strings.Count(l.input[l.start:l.pos], "\n")
	l.start = l.pos
	l.startLine = l.line
}

// accept consumes the next rune if it's from the valid set.
func (l *lexer) accept(valid string) bool {
	if strings.ContainsRune(valid, l.next()) {
		return true
	}
	l.backup()
	return false
}

// acceptRun consumes a run of runes from the valid set.
func (l *lexer) acceptRun(valid string) {
	for strings.ContainsRune(valid, l.next()) {
	}
	l.backup()
}

// errorf returns an error token and terminates the scan by passing
// back a nil pointer that will be the next state, terminating l.nextItem.
func (l *lexer) errorf(format string, args ...interface{}) stateFn {
	l.items <- item{itemError, l.start, fmt.Sprintf(format, args...), l.startLine}
	return nil
}

// nextItem returns the next item from the input.
// Called by the parser, not in the lexing goroutine.
func (l *lexer) nextItem() item {
	return <-l.items
}

// drain drains the output so the lexing goroutine will exit.
// Called by the parser, not in the lexing goroutine.
func (l *lexer) drain() {
	for range l.items {
	}
}

// lex creates a new scanner for the input string.
func lex(name, input string) *lexer {
	l := &lexer{
		name:      name,
		input:     input,
		items:     make(chan item),
		line:      1,
		startLine: 1,
	}
	go l.run(lexStartStatement)
	return l
}

// run runs the state machine for the lexer.
func (l *lexer) run(start stateFn) {
	for state := start; state != nil; {
		state = state(l)
	}
	close(l.items)
}

func lexStartStatement(l *lexer) stateFn {
	l.skipWhitespace()
	switch r := l.next(); {
	case r == eof:
		return l.errorf("not finished statement")

	case r == 's' || r == 'S':
		l.backup()
		return lexSelectKeyword
	default:
		return l.errorf("unrecognized character at start of statement: %#U", r)
	}
}

func createLexKeyword(keyword string, it itemType, nextAction stateFn) stateFn {
	lower := []rune(strings.ToLower(keyword))
	upper := []rune(strings.ToUpper(keyword))

	return func(l *lexer) stateFn {
		l.skipWhitespace()
		for index, lowerCaseRune := range lower {
			upperCaseRune := upper[index]
			r := l.next()
			if !(r == lowerCaseRune || r == upperCaseRune) {
				return l.errorf("expected %c or %c at pos %v in keyword %v, but got <%c>", lowerCaseRune, upperCaseRune, index, keyword, r)
			}
		}
		l.emit(it)
		return nextAction
	}
}

var lexSelectKeyword = createLexKeyword("select", itemSelect, lexValueExprList)
var lexFromKeyword = createLexKeyword("from", itemFrom, lexFromTableName)

func lexValueExprList(l *lexer) stateFn {
	l.skipWhitespace()
	switch r := l.next(); {
	case r == '*':
		l.emit(itemStar)
	case r == '\'':
		l.backup()
		if !l.scanString() {
			return l.errorf("bad string: %q", l.input[l.start:l.pos])
		}
		l.emit(itemString)
	case r == '+' || r == '-' || ('0' <= r && r <= '9'):
		l.backup()
		if !l.scanNumber() {
			return l.errorf("bad number: %q", l.input[l.start:l.pos])
		}
		l.emit(itemNumber)
	case isAlpha(r):
		l.backup()
		if !l.scanIdentifier() {
			return l.errorf("bad identifier: %q", l.input[l.start:l.pos])
		}
		l.emit(itemIdentifier)
	default:
		return l.errorf("unrecognized character at value exrt: %#U", r)
	}

	l.skipWhitespace()

	switch r := l.next(); {
	case r == ',':
		l.emit(itemComma)
		return lexValueExprList
	case r == 'f' || r == 'F':
		l.backup()
		return lexFromKeyword
	default:
		return l.errorf("unrecognized character after value expr: %#U", r)
	}
}

func lexFromTableName(l *lexer) stateFn {
	l.skipWhitespace()
	if !l.scanIdentifier() {
		return l.errorf("bad identifier: %q", l.input[l.start:l.pos])
	}
	l.emit(itemIdentifier)
	return lexEndOfStatement
}

func lexEndOfStatement(l *lexer) stateFn {
	l.skipWhitespace()
	if l.accept(";") {
		l.emit(itemEOF)
	} else {
		return l.errorf("unterminated statement")
	}

	return nil
}

func (l *lexer) scanIdentifier() bool {
	chars := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_"
	charsAndDigits := chars + "0123456789"
	// first character should not contain digits
	if !l.accept(chars) {
		return false
	}
	// next could be anything
	l.acceptRun(charsAndDigits)
	// Next thing mustn't be alphanumeric.

	if isAlphaNumeric(l.peek()) {
		l.next()
		return false
	}
	return true
}

func (l *lexer) skipWhitespace() {
	l.acceptRun(spaceChars)
	l.start = l.pos
}

func (l *lexer) scanNumber() bool {
	// Optional leading sign.
	l.accept("+-")
	// Is it hex?
	digits := "0123456789_"
	if l.accept("0") {
		// Note: Leading 0 does not mean octal in floats.
		if l.accept("xX") {
			digits = "0123456789abcdefABCDEF_"
		} else if l.accept("oO") {
			digits = "01234567_"
		} else if l.accept("bB") {
			digits = "01_"
		}
	}
	l.acceptRun(digits)
	if l.accept(".") {
		l.acceptRun(digits)
	}
	if len(digits) == 10+1 && l.accept("eE") {
		l.accept("+-")
		l.acceptRun("0123456789_")
	}
	if len(digits) == 16+6+1 && l.accept("pP") {
		l.accept("+-")
		l.acceptRun("0123456789_")
	}
	// Is it imaginary?
	l.accept("i")
	// Next thing mustn't be alphanumeric.
	if isAlphaNumeric(l.peek()) {
		l.next()
		return false
	}
	return true
}

func (l *lexer) scanString() bool {
	if r := l.next(); r != '\'' {
		l.backup()
		return false
	}
	for {
		switch l.next() {
		case '\\':
			if r := l.next(); r != eof && r != '\n' {
				break
			}
			fallthrough
		case eof, '\n':
			return false
		case '\'':
			if r := l.next(); r != '\'' {
				l.backup()
				return true
			}

		}
	}
}

// isSpace reports whether r is a space character.
func isSpace(r rune) bool {
	return r == ' ' || r == '\t'
}

// isEndOfLine reports whether r is an end-of-line character.
func isEndOfLine(r rune) bool {
	return r == '\r' || r == '\n'
}

// isAlphaNumeric reports whether r is an alphabetic, digit, or underscore.
func isAlphaNumeric(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isAlpha(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isDigit(r rune) bool {
	return ('0' <= r && r <= '9')
}

func main() {
	l := lex("TEST SELECTS", " sElect col  , col, 11, 'ab''c' from table;")

read:
	for {
		switch i := l.nextItem(); {
		case i.typ == itemError:
			fmt.Println("ERROR", i)
			break read
		case i.typ == itemEOF:
			fmt.Println("END")
			break read
		default:
			fmt.Printf("%v %v\n", i, i.typ)
		}
	}

}
