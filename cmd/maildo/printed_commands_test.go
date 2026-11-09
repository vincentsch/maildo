package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/vincentsch/rungrad/testutil"
)

// Text that a shell treats specially: line breaks, both kinds of quotes,
// backslashes, $ and backticks, a leading dash, and letters outside ASCII.
const (
	trickyBody    = "Hi Maria,\n\nthank you for the \"final\" logo files!\nIt's grüß Gott 🎉, $HOME `id` $(echo no) and a literal \\n.\nA backslash at the end \\\n\nBest,\nSam\n"
	trickySubject = "Maria's \"logo\" files, grüß $USER"
	trickyTo      = "\"O'Brien, Pat\" <pat@example.org>"
	trickyFolder  = "-Kunden 'Müller' \"Q4\" $x"
)

// shellArgs runs a command maildo printed through sh, the way a person or
// an agent pastes it, and returns the arguments sh hands to maildo. A
// shell function named maildo stands in for the program, so nothing else
// runs and any text that escaped its quotes shows up in the output.
func shellArgs(t *testing.T, command string) []string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell to paste the command into")
	}
	if !strings.HasPrefix(command, "maildo ") {
		t.Fatalf("printed command does not start with maildo: %q", command)
	}
	script := "maildo() { for a in \"$@\"; do printf '%s\\0' \"$a\"; done; }\n" + command
	out, err := exec.Command(sh, "-c", script).Output()
	if err != nil {
		t.Fatalf("sh could not run %q: %v", command, err)
	}
	if len(out) == 0 || out[len(out)-1] != 0 {
		t.Fatalf("sh printed more than the arguments of %q: %q", command, out)
	}
	return strings.Split(string(out[:len(out)-1]), "\x00")
}

// runThroughShell runs a printed command as the shell would pass it on.
func runThroughShell(t *testing.T, run func(...string) testutil.Result, command string) testutil.Result {
	t.Helper()
	return run(shellArgs(t, command)...)
}

// commandAfter returns the command printed after marker in human output.
// It may span lines, when an argument has line breaks; it ends at the
// first line break outside quotes.
func commandAfter(t *testing.T, out, marker string) string {
	t.Helper()
	_, rest, ok := strings.Cut(out, marker+"\n  ")
	if !ok {
		t.Fatalf("no command after %q in:\n%s", marker, out)
	}
	var quote rune
	for i, r := range rest {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '\n':
			return rest[:i]
		}
	}
	return rest
}

func TestShellQuoteKeepsEveryCharacter(t *testing.T) {
	var all strings.Builder
	for r := rune(1); r < 128; r++ {
		all.WriteRune(r)
	}
	values := []string{"", "plain", "-dash", "--json", "it's", "''", `"`, `\`, "a\nb", "a\r\nb", "tab\there", "~", "#x", "a b", "*", "!x", "$(id)", "`id`", "grüß 🎉", "\u202eevil", all.String(), trickyBody, trickySubject, trickyTo, trickyFolder}
	words := make([]string, len(values))
	for i, v := range values {
		words[i] = shellQuote(v)
	}
	got := shellArgs(t, "maildo "+strings.Join(words, " "))
	if len(got) != len(values) {
		t.Fatalf("got %d arguments, want %d: %q", len(got), len(values), got)
	}
	for i, v := range values {
		if got[i] != v {
			t.Errorf("shellQuote(%q) came back as %q", v, got[i])
		}
	}
}

// Every command maildo prints for the next step must give maildo the same
// arguments when pasted into a shell, in the human output and in the JSON,
// and must then do what the preview showed.
func TestPrintedCommandsSurviveTheShell(t *testing.T) {
	t.Run("send", func(t *testing.T) {
		h := newSendHarness(t)
		args := []string{"send", "--to", trickyTo, "--subject", trickySubject, "--body", trickyBody, "--dry-run"}
		var preview sendPreview
		decodeOK(t, h.run(append(args, "--json")...), &preview)
		checkSendCommand(t, h, args, preview.SendCommand)
		if len(h.server.Sent()) != 1 {
			t.Fatalf("sent %d messages, want 1", len(h.server.Sent()))
		}
	})

	t.Run("reply and forward", func(t *testing.T) {
		h := newSendHarness(t)
		h.server.Append("INBOX", "From: Maria <maria@example.org>\r\nTo: me@posteo.de\r\nSubject: Logo files\r\nMessage-ID: <logo@example.org>\r\n\r\nHere they are.\r\n", time.Now())
		id := searchOne(t, h, "subject:logo", "INBOX")
		for _, args := range [][]string{
			{"reply", "--id", id, "--send", "--text", "--body", trickyBody, "--dry-run"},
			{"forward", "--id", id, "--send", "--to", trickyTo, "--body", trickyBody, "--dry-run"},
		} {
			var preview sendPreview
			decodeOK(t, h.run(append(args, "--json")...), &preview)
			checkSendCommand(t, h, args, preview.SendCommand)
		}
		if len(h.server.Sent()) != 2 {
			t.Fatalf("sent %d messages, want 2", len(h.server.Sent()))
		}
	})

	t.Run("draft create and update", func(t *testing.T) {
		h := newSendHarness(t)
		create := h.run("draft", "create", "--to", trickyTo, "--subject", trickySubject, "--body", trickyBody, "--dry-run")
		command := commandAfter(t, create.Stdout, "To save it, run:")
		got := shellArgs(t, command)
		if flagValue(got, "--body") != trickyBody || flagValue(got, "--subject") != trickySubject || flagValue(got, "--to") != trickyTo {
			t.Fatalf("the shell passed %q", got)
		}
		var saved draftWriteResult
		decodeOK(t, h.run(append(got, "--json")...), &saved)

		update := h.run("draft", "update", "--id", saved.DraftID, "--body", trickyBody+"P.S.\n", "--dry-run")
		got = shellArgs(t, commandAfter(t, update.Stdout, "To update the draft, run:"))
		if flagValue(got, "--body") != trickyBody+"P.S.\n" {
			t.Fatalf("the shell passed %q", got)
		}
		if res := h.run(got...); res.Exit != 0 {
			t.Fatalf("draft update: %s%s", res.Stdout, res.Stderr)
		}
	})

	t.Run("folders, moves and undo", func(t *testing.T) {
		h := newSendHarness(t)
		create := h.run("folders", "create", "--dry-run", "--", trickyFolder)
		got := shellArgs(t, commandAfter(t, create.Stdout, "To create it, run:"))
		if got[len(got)-1] != trickyFolder {
			t.Fatalf("the shell passed %q", got)
		}
		if res := h.run(got...); res.Exit != 0 {
			t.Fatalf("folders create: %s%s", res.Stdout, res.Stderr)
		}

		// A message moved out of INBOX comes back with the undo command,
		// from the JSON and from the text output.
		h.server.Append("INBOX", "From: shop@example.org\r\nSubject: Alpha\r\nMessage-ID: <alpha@example.org>\r\n\r\nbody\r\n", time.Now())
		var moved mutationResult
		decodeOK(t, h.run("move", "--id", searchOne(t, h, "subject:alpha", "INBOX"), "--to", trickyFolder, "--json"), &moved)
		if len(moved.UndoCommands) != 1 {
			t.Fatalf("undo commands = %q", moved.UndoCommands)
		}
		if res := runThroughShell(t, h.run, moved.UndoCommands[0]); res.Exit != 0 {
			t.Fatalf("undo from JSON: %s%s", res.Stdout, res.Stderr)
		}
		human := h.run("move", "--id", searchOne(t, h, "subject:alpha", "INBOX"), "--to", trickyFolder)
		if res := runThroughShell(t, h.run, commandAfter(t, human.Stdout, "To move it back to INBOX, run:")); res.Exit != 0 {
			t.Fatalf("undo from the text output: %s%s", res.Stdout, res.Stderr)
		}
		searchOne(t, h, "subject:alpha", "INBOX")

		// A message moved out of the folder goes back into it.
		h.server.Append(trickyFolder, "From: shop@example.org\r\nSubject: Beta\r\nMessage-ID: <beta@example.org>\r\n\r\nbody\r\n", time.Now())
		h.server.Append(trickyFolder, "From: shop@example.org\r\nSubject: Gamma\r\nMessage-ID: <gamma@example.org>\r\n\r\nbody\r\n", time.Now())
		decodeOK(t, h.run("move", "--id", searchOne(t, h, "subject:beta", trickyFolder), "--to", "inbox", "--json"), &moved)
		if res := runThroughShell(t, h.run, moved.UndoCommands[0]); res.Exit != 0 {
			t.Fatalf("undo into the folder: %s%s", res.Stdout, res.Stderr)
		}
		searchOne(t, h, "subject:beta", trickyFolder)

		// The next page command of a list names the folder.
		list := h.run("list", "--limit", "1", "--", trickyFolder)
		_, next, ok := strings.Cut(list.Stdout, "Next page: ")
		if !ok {
			t.Fatalf("list has no next page:\n%s", list.Stdout)
		}
		if res := runThroughShell(t, h.run, strings.TrimSuffix(next, "\n")); res.Exit != 0 || !strings.Contains(res.Stdout, "Showing 2-2") {
			t.Fatalf("next page: %s%s", res.Stdout, res.Stderr)
		}
	})

	t.Run("bulk change", func(t *testing.T) {
		h := newSendHarness(t)
		for _, n := range []string{"one", "two"} {
			h.server.Append("INBOX", "From: news@shop.example\r\nSubject: News "+n+"\r\nMessage-ID: <"+n+"@shop.example>\r\n\r\nbody\r\n", time.Now())
		}
		if res := h.run("folders", "create", "--", trickyFolder); res.Exit != 0 {
			t.Fatalf("folders create: %s", res.Stderr)
		}
		args := []string{"move", "--query", "from:news@shop.example", "--all", "--to", trickyFolder}
		var preview mutationPreview
		decodeOK(t, h.run(append(args, "--json")...), &preview)
		human := shellArgs(t, commandAfter(t, h.run(args...).Stdout, "$x, run:"))
		fromJSON := strings.Join(shellArgs(t, preview.ApplyCommand), "\x00")
		if strings.Join(human, "\x00") != strings.Replace(fromJSON, "\x00--json", "", 1) {
			t.Fatalf("the text output has the command %q, the JSON %q", human, preview.ApplyCommand)
		}
		var result bulkMutationResult
		decodeOK(t, runThroughShell(t, h.run, preview.ApplyCommand), &result)
		if result.Counts.Succeeded != 2 || len(result.UndoCommands) != 1 {
			t.Fatalf("result = %+v", result)
		}
		if res := runThroughShell(t, h.run, result.UndoCommands[0]); res.Exit != 0 {
			t.Fatalf("undo: %s%s", res.Stdout, res.Stderr)
		}
	})
}

// checkSendCommand checks one send preview's command in all three outputs,
// pastes it into a shell and runs it. The send only works when the shell
// handed over exactly the previewed message, because the approval code
// covers every byte of it.
func checkSendCommand(t *testing.T, h *mailServerHarness, args []string, command string) {
	t.Helper()
	human := shellArgs(t, commandAfter(t, h.run(args...).Stdout, "To send it, run:"))
	if flagValue(human, "--body") != trickyBody {
		t.Fatalf("the shell passed the body %q from the text output", flagValue(human, "--body"))
	}
	if jq := h.run(append(args, "--jq", ".send_command")...); jq.Stdout != command+"\n" {
		t.Fatalf("--jq .send_command = %q, want %q", jq.Stdout, command)
	}
	got := shellArgs(t, command)
	if flagValue(got, "--body") != trickyBody {
		t.Fatalf("the shell passed the body %q", flagValue(got, "--body"))
	}
	if res := h.run(got...); res.Exit != 0 {
		t.Fatalf("%s: %s%s", args[0], res.Stdout, res.Stderr)
	}
}

// A tab or another control character does not survive a copy from the
// terminal, so the text output says what to do instead of printing a
// command that would do something else.
func TestCommandsWithControlCharactersAreNotPrinted(t *testing.T) {
	h := newSendHarness(t)
	draft := h.run("draft", "create", "--to", "pat@example.org", "--body", "Name\tAmount\n", "--dry-run")
	if !strings.Contains(draft.Stdout, "To save it, run:\n  (The command has characters a terminal cannot show. Run the same command again without --dry-run.)\n") {
		t.Fatalf("draft preview:\n%s", draft.Stdout)
	}
	send := h.run("send", "--to", "pat@example.org", "--subject", "Totals", "--body", "Name\tAmount\n", "--dry-run")
	if !strings.Contains(send.Stdout, "To send it, run:\n  (The command has characters a terminal cannot show. Add --json to get it.)\n") {
		t.Fatalf("send preview:\n%s", send.Stdout)
	}
	var preview sendPreview
	decodeOK(t, h.run("send", "--to", "pat@example.org", "--subject", "Totals", "--body", "Name\tAmount\n", "--dry-run", "--json"), &preview)
	if got := shellArgs(t, preview.SendCommand); flagValue(got, "--body") != "Name\tAmount\n" {
		t.Fatalf("the shell passed %q", got)
	}
}
