package main

import (
	"bufio"
	"fmt"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/vincentsch/rungrad"
	"github.com/vincentsch/rungrad/resolve"
)

// promptSession reads answers from the Factory's stdin, so tests can give
// the answers and pretend to be a terminal.
type promptSession struct {
	f      *rungrad.Factory
	cmd    *cobra.Command
	reader *bufio.Reader
}

func newPromptSession(f *rungrad.Factory, cmd *cobra.Command) *promptSession {
	return &promptSession{f: f, cmd: cmd, reader: stdinReader(f)}
}

// stdinReaders holds one buffered reader per command run. Login asks some
// questions itself and lets the provider ask others; with two readers, the
// first could swallow a line meant for the second (a pasted block, or a
// test's input).
var stdinReaders sync.Map

func stdinReader(f *rungrad.Factory) *bufio.Reader {
	if r, ok := stdinReaders.Load(f); ok {
		return r.(*bufio.Reader)
	}
	r, _ := stdinReaders.LoadOrStore(f, bufio.NewReader(f.Stdin))
	return r.(*bufio.Reader)
}

// forgetStdinReader drops the run's reader when the command is done.
func forgetStdinReader(f *rungrad.Factory) {
	stdinReaders.Delete(f)
}

// line asks once. It returns def when the user presses Enter without an
// answer, or when there is nobody to ask.
func (p *promptSession) line(label, def string) string {
	if !canPrompt(p.f, p.cmd) {
		return def
	}
	fmt.Fprint(p.f.Stderr, label+" ")
	text, err := p.reader.ReadString('\n')
	if err != nil && text == "" {
		return def
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return def
	}
	return text
}

// canPrompt follows rungrad's rule for when maildo may ask a question. With
// --json, --dry-run or --no-prompt, maildo never waits for input.
func canPrompt(f *rungrad.Factory, cmd ...*cobra.Command) bool {
	if f == nil || f.Flags == nil {
		return false
	}
	if f.Flags.NoPrompt || f.DryRun() || machineMode(f, cmd...) {
		return false
	}
	if f.PromptTerminalSet {
		return f.PromptTerminal
	}
	return resolve.IsInteractive(f.Flags.NoPrompt)
}

// machineMode reports whether the command prints JSON, --jq or --template
// output instead of text for people.
func machineMode(f *rungrad.Factory, cmd ...*cobra.Command) bool {
	if f == nil || f.Flags == nil {
		return false
	}
	if f.Flags.JSON {
		return true
	}
	for _, c := range cmd {
		if c == nil {
			continue
		}
		flags := c.Flags()
		return flags.Changed("jq") || flags.Changed("template")
	}
	return f.Flags.JQ != "" || f.Flags.Template != ""
}
