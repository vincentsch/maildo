package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/maildo/internal/testkit"
)

// collidingIDs finds two IMAP ids in account work whose 7-character short
// ids are the same.
func collidingIDs(t *testing.T) (string, string) {
	t.Helper()
	seen := map[string]string{}
	for n := 1; n < 500000; n++ {
		id := fmt.Sprintf("imap-message/v1/SU5CT1g/1/%d", n)
		s := shortID("work", id)
		if previous, ok := seen[s]; ok {
			return previous, id
		}
		seen[s] = id
	}
	t.Fatal("no collision found")
	return "", ""
}

// printedID returns the ID line of read's human output.
func printedID(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(line, "ID:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("no ID line in:\n%s", stdout)
	return ""
}

// A short id printed for a message that later fell out of the cache must
// not start pointing at a newer message with the same start.
func TestForgottenShortIDIsNeverHandedOutAgain(t *testing.T) {
	a, b := collidingIDs(t)
	sample := testkit.SampleMessages()["work"][0]
	sa, sb := sample, sample
	sa.ID, sa.Subject = a, "OLD message"
	sb.ID, sb.Subject = b, "NEW message"
	full := testkit.SampleFullMessages()["work"]["work-new"]
	fa, fb := full, full
	fa.ID, fa.Subject = a, sa.Subject
	fb.ID, fb.Subject = b, sb.Subject
	fake := testkit.NewFakeMutationProvider("fake", map[string][]mail.MessageSummary{"work": {sa, sb}}, map[string]map[string]mail.Message{"work": {a: fa, b: fb}})
	h, _ := seedMutationHarness(t, fake)

	readA := h.run("read", a)
	decodeOK(t, readA, nil)
	oldShort := printedID(t, readA.Stdout)
	if oldShort != shortID("work", a) {
		t.Fatalf("short id of the old message = %q", oldShort)
	}

	// Show 5,000 other messages, as list and search would, so the old
	// message falls out of the cache.
	cache := filepath.Join(filepath.Dir(h.configPath), shortIDsFile)
	others := make([]shortIDEntry, maxShortIDs)
	for i := range others {
		others[i] = messageRef("work", fmt.Sprintf("work-other-%d", i))
	}
	if _, err := saveShortIDs(cache, others); err != nil {
		t.Fatal(err)
	}
	if file := loadShortIDs(cache); len(file.Forgotten) != 1 || file.Forgotten[0] != forgottenHash("work", a) {
		t.Fatalf("forgotten = %v", file.Forgotten)
	}

	readB := h.run("read", b)
	decodeOK(t, readB, nil)
	newShort := printedID(t, readB.Stdout)
	if len(newShort) <= len(oldShort) || !strings.HasPrefix(newShort, oldShort) {
		t.Fatalf("new message got short id %q, old one had %q", newShort, oldShort)
	}

	res := h.run("archive", oldShort, "--json")
	assertErrorCode(t, res, errs.CodeInvalidSelector)
	if !strings.Contains(res.Stderr, "no longer remembers which message short id "+oldShort+" stood for") || !strings.Contains(res.Stderr, "maildo list") {
		t.Fatalf("refusal = %s", res.Stderr)
	}
	if fake.MessageMutationCalls.Load() != 0 {
		t.Fatalf("the old short id changed a message: %v", fake.MutationCallLog)
	}

	decodeOK(t, h.run("archive", newShort, "--json"), nil)
	if len(fake.MutationCallLog) != 1 || !strings.Contains(fake.MutationCallLog[0], b) {
		t.Fatalf("mutations = %v", fake.MutationCallLog)
	}
}

// A forgotten message that is shown again is remembered again, and its
// new short id works.
func TestShownAgainLeavesTheForgottenList(t *testing.T) {
	cache := filepath.Join(t.TempDir(), shortIDsFile)
	first := messageRef("work", "work-first")
	if _, err := saveShortIDs(cache, []shortIDEntry{first}); err != nil {
		t.Fatal(err)
	}
	others := make([]shortIDEntry, maxShortIDs)
	for i := range others {
		others[i] = messageRef("work", fmt.Sprintf("work-other-%d", i))
	}
	if _, err := saveShortIDs(cache, others); err != nil {
		t.Fatal(err)
	}
	if got := loadShortIDs(cache).Forgotten; len(got) != 1 {
		t.Fatalf("forgotten = %v", got)
	}
	ids, err := saveShortIDs(cache, []shortIDEntry{first})
	if err != nil {
		t.Fatal(err)
	}
	file := loadShortIDs(cache)
	if len(file.Forgotten) != 1 || file.Forgotten[0] == forgottenHash("work", "work-first") {
		t.Fatalf("forgotten after showing it again = %v", file.Forgotten)
	}
	if got := ids.of("work", "work-first"); got != shortID("work", "work-first") {
		t.Fatalf("short id = %q", got)
	}
}

// Commands that print tables at the same time must not drop each other's
// short ids.
func TestConcurrentSavesKeepEveryShortID(t *testing.T) {
	cache := filepath.Join(t.TempDir(), shortIDsFile)
	const writers = 24
	var wg sync.WaitGroup
	errsCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := saveShortIDs(cache, []shortIDEntry{messageRef("work", fmt.Sprintf("work-concurrent-%d", i))})
			errsCh <- err
		}(i)
	}
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	file := loadShortIDs(cache)
	if len(file.Entries) != writers {
		t.Fatalf("cache has %d entries, want %d", len(file.Entries), writers)
	}
}
