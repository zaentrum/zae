package addon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// errInterrupted: Ctrl-C or SIGTERM ended the command.
var errInterrupted = errors.New("interrupted")

// notifySignals subscribes c to the signals that end a command. Tests
// substitute it to deliver one on cue.
var notifySignals = func(c chan<- os.Signal) (stop func()) {
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	return func() { signal.Stop(c) }
}

// session is one command's lifetime: a context that ends on Ctrl-C or
// SIGTERM, so a wait, a question or a request stops where it is and the
// command can put things back before it exits.
type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	sig    os.Signal
}

func newSession() *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{ctx: ctx, cancel: cancel}
	ch := make(chan os.Signal, 1)
	stop := notifySignals(ch)
	go func() {
		defer stop()
		select {
		case sig := <-ch:
			s.mu.Lock()
			s.sig = sig
			s.mu.Unlock()
			cancel()
		case <-ctx.Done():
		}
	}()
	return s
}

// close ends the session; every command defers it.
func (s *session) close() { s.cancel() }

// interrupted reports whether a signal ended the session.
func (s *session) interrupted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sig != nil
}

// exitCode is the conventional status for the signal that ended the session,
// 128 plus its number: 130 for Ctrl-C, 143 for SIGTERM.
func (s *session) exitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sig, ok := s.sig.(syscall.Signal); ok {
		return 128 + int(sig)
	}
	return 130
}

// One buffered reader serves every question a command asks on stdin, so an
// answer typed ahead is not lost between two questions.
var (
	linesMu   sync.Mutex
	linesFrom io.Reader
	lines     *bufio.Reader
)

func lineReader() *bufio.Reader {
	linesMu.Lock()
	defer linesMu.Unlock()
	if lines == nil || linesFrom != stdin {
		linesFrom, lines = stdin, bufio.NewReader(stdin)
	}
	return lines
}

// readLine reads one line from stdin, or stops waiting for it when the
// session ends.
func (s *session) readLine() (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := lineReader().ReadString('\n')
		done <- result{line, err}
	}()
	select {
	case r := <-done:
		if r.err != nil && r.line == "" {
			return "", r.err
		}
		return strings.TrimRight(r.line, "\r\n"), nil
	case <-s.ctx.Done():
		return "", errInterrupted
	}
}

// confirm asks on stdin. Anything but y or yes is no — an empty line and a
// closed stdin included — so a stray Enter never installs or removes.
func (s *session) confirm(question string) (bool, error) {
	fmt.Fprintf(stdout, "%s [y/N] ", question)
	line, err := s.readLine()
	if err != nil {
		fmt.Fprintln(stdout)
		if errors.Is(err, errInterrupted) {
			return false, err
		}
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// readSecret asks for one secret input on the terminal with echo turned off.
// Tests substitute it.
var readSecret = func(s *session, prompt string) (string, error) {
	restore, err := withoutEcho(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("cannot turn off echo on this terminal (%v) — give the secret with --set-secret-file or --secret-values", err)
	}
	fmt.Fprint(stderr, prompt)
	line, err := s.readLine()
	restore()
	fmt.Fprintln(stderr)
	return line, err
}

// stdinIsTerminal reports whether a person can answer on stdin.
func stdinIsTerminal() bool { return terminal(os.Stdin) }
