package initwiz

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrInterrupted: Ctrl-C at a prompt.
var ErrInterrupted = errors.New("прервано")

// errNoTerminal: interactive mode without a terminal on stdin.
var errNoTerminal = errors.New("нет терминала для ввода: запустите в терминале (docker run -it ...) или используйте --non-interactive с переменными окружения")

// Terminal is the prompt side of init.
type Terminal interface {
	// Line asks and reads one line (trimmed).
	Line(prompt string) (string, error)
	// Secret asks and reads one line without echo; the value never reaches
	// the output.
	Secret(prompt string) (string, error)
}

// TTY is the real terminal: lines from In, prompts to Out; Secret turns
// the echo off with stty (no extra dependency) and back on, Ctrl-C
// included.
type TTY struct {
	In  io.Reader
	Out io.Writer
	// Stty runs stty on the terminal (nil = no terminal: Secret fails).
	Stty func(args ...string) (string, error)
	// Interrupt delivers Ctrl-C while a secret is read (nil = never).
	Interrupt <-chan os.Signal

	r *bufio.Reader
}

func (t *TTY) reader() *bufio.Reader {
	if t.r == nil {
		t.r = bufio.NewReader(t.In)
	}
	return t.r
}

func (t *TTY) readLine() (string, error) {
	s, err := t.reader().ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", ErrInterrupted // Ctrl-D: the input ended
		}
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// Line implements Terminal.
func (t *TTY) Line(prompt string) (string, error) {
	fmt.Fprint(t.Out, prompt)
	return t.waitLine()
}

// waitLine reads a line or returns ErrInterrupted on Ctrl-C (init catches
// SIGINT for the whole run, so a blocked read must watch for it too).
func (t *TTY) waitLine() (string, error) {
	type line struct {
		s   string
		err error
	}
	got := make(chan line, 1)
	go func() {
		s, err := t.readLine()
		got <- line{s, err}
	}()
	select {
	case l := <-got:
		return l.s, l.err
	case <-t.Interrupt:
		fmt.Fprintln(t.Out)
		return "", ErrInterrupted
	}
}

// Secret implements Terminal.
func (t *TTY) Secret(prompt string) (string, error) {
	if t.Stty == nil {
		return "", errNoTerminal
	}
	saved, err := t.Stty("-g")
	if err != nil {
		return "", errNoTerminal
	}
	if _, err := t.Stty("-echo"); err != nil {
		return "", errNoTerminal
	}
	restore := func() {
		_, _ = t.Stty(strings.TrimSpace(saved))
		fmt.Fprintln(t.Out) // the Enter the user typed was not echoed
	}
	fmt.Fprint(t.Out, prompt)
	s, err := t.waitLine()
	restore()
	return s, err
}
