package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/fsutil"
	"github.com/vincentsch/rungrad"
)

// Short ids
//
// Provider message ids are long (an IMAP id holds the folder name, the
// folder's UIDVALIDITY and the UID). Tables show a short id instead: the
// start of a SHA-256 of the account name and the full id, 7 hex characters
// unless two messages maildo knows about start the same way. Then both get
// more characters until they differ. The same message always gets the
// same hash, so its short id only grows, never changes.
//
// A hash cannot be turned back into an id, so maildo remembers the messages
// it showed in short-ids.json next to the config file, newest first, up to
// maxShortIDs entries. Any --id flag, and the id argument of read and
// thread, accepts a short id from there. A short id that matches two
// remembered messages is refused with both candidates; maildo never picks
// one. JSON output always carries the full ids.
//
// When a message falls out of the list, maildo still remembers the first
// 11 characters of its hash in "forgotten". A short id printed for it
// earlier is then refused instead of pointing at a newer message that
// starts the same way, and new messages get short ids that cannot be
// mistaken for it. The forgotten list is never trimmed. It only grows with
// the number of different messages maildo has shown, at about 14 bytes
// each, so a short id is never handed out twice.
//
// Commands that print tables can run at the same time, so the cache is
// read, merged and written under a lock on short-ids.lock.

const (
	shortIDLength = 7
	// Gmail message ids are 16 hex characters, so short ids stop well below.
	maxShortIDLength = 11
	maxShortIDs      = 5000
	shortIDsFile     = "short-ids.json"
	shortIDsLockFile = "short-ids.lock"
	shortIDVersion   = 3
	// shortIDLockWait is how long a command waits for another one to finish
	// writing the cache before it gives up on saving.
	shortIDLockWait = 2 * time.Second
)

var shortIDPattern = regexp.MustCompile(`^[0-9a-f]{7,11}$`)

type shortIDEntry struct {
	Short   string `json:"short"`
	Account string `json:"account"`
	ID      string `json:"id"`
	// Subject helps tell two messages apart when a short id matches both.
	Subject string `json:"subject,omitempty"`
}

func (e shortIDEntry) key() string { return e.Account + "\n" + e.ID }

type shortIDFile struct {
	Version int            `json:"version"`
	Entries []shortIDEntry `json:"entries"`
	// Forgotten holds the first maxShortIDLength hash characters of
	// messages that fell out of Entries.
	Forgotten []string `json:"forgotten,omitempty"`
}

// forgottenHash is what the forgotten list keeps of a message.
func forgottenHash(account, id string) string {
	return idHash(account, id)[:maxShortIDLength]
}

// idHash is the full hex SHA-256 behind a short id.
func idHash(account, id string) string {
	sum := sha256.Sum256([]byte(account + "\n" + id))
	return hex.EncodeToString(sum[:])
}

// shortID is the 7-character form of a message's short id. Tables use
// shortIDs.of, which adds characters when two messages collide.
func shortID(account, id string) string {
	return idHash(account, id)[:shortIDLength]
}

// isShortID reports whether s has the shape of a short id. No provider uses
// 7 to 11 lowercase hex characters as a message id.
func isShortID(s string) bool {
	return shortIDPattern.MatchString(s)
}

func shortIDPath(f *rungrad.Factory) string {
	return filepath.Join(filepath.Dir(f.ConfigPath()), shortIDsFile)
}

func loadShortIDs(path string) shortIDFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return shortIDFile{Version: shortIDVersion}
	}
	var file shortIDFile
	if json.Unmarshal(data, &file) != nil || file.Version != shortIDVersion {
		return shortIDFile{Version: shortIDVersion}
	}
	return file
}

// shortIDs maps the messages of one table to the short ids it shows.
type shortIDs map[string]string

// of returns the short id to show for a message.
func (s shortIDs) of(account, id string) string {
	if short, ok := s[account+"\n"+id]; ok {
		return short
	}
	return shortID(account, id)
}

// assignShortIDs gives each message in refs the shortest id, from 7
// characters up, that no other message in refs or in known starts with,
// and that is not the start of a forgotten message's hash.
func assignShortIDs(refs, known []shortIDEntry, forgotten []string) shortIDs {
	hashes := map[string]string{}
	for _, list := range [][]shortIDEntry{known, refs} {
		for _, e := range list {
			if e.ID != "" {
				hashes[e.key()] = idHash(e.Account, e.ID)
			}
		}
	}
	// Group by the 7-character prefix; only groups with more than one
	// message need longer ids.
	groups := map[string][]string{}
	for key, h := range hashes {
		groups[h[:shortIDLength]] = append(groups[h[:shortIDLength]], key)
	}
	gone := map[string][]string{}
	for _, f := range forgotten {
		if len(f) == maxShortIDLength {
			gone[f[:shortIDLength]] = append(gone[f[:shortIDLength]], f)
		}
	}
	out := shortIDs{}
	for _, ref := range refs {
		if ref.ID == "" {
			continue
		}
		key := ref.key()
		h := hashes[key]
		n := shortIDLength
		for _, other := range groups[h[:shortIDLength]] {
			if other == key {
				continue
			}
			for n < maxShortIDLength && h[:n] == hashes[other][:n] {
				n++
			}
		}
		for _, f := range gone[h[:shortIDLength]] {
			// Skip the message's own hash from when it was forgotten.
			if f == h[:maxShortIDLength] {
				continue
			}
			for n < maxShortIDLength && h[:n] == f[:n] {
				n++
			}
		}
		out[key] = h[:n]
	}
	return out
}

// rememberShortIDs works out the short ids for a table and adds the
// messages to the cache. It saves only for human and plain output, where
// short ids are shown, and never on --dry-run. A failure to save is not
// worth failing the command for, so it becomes a note on stderr.
func rememberShortIDs(f *rungrad.Factory, refs []shortIDEntry) shortIDs {
	if len(refs) == 0 || f.Flags == nil || f.Flags.JSON || f.Flags.JQ != "" || f.Flags.Template != "" {
		return shortIDs{}
	}
	path := shortIDPath(f)
	if f.Flags.DryRun {
		file := loadShortIDs(path)
		return assignShortIDs(refs, file.Entries, file.Forgotten)
	}
	ids, err := saveShortIDs(path, refs)
	if err != nil {
		f.Infof("Warning: could not save the short ids: %v", err)
	}
	return ids
}

// saveShortIDs works out the short ids for refs and adds them to the cache
// at path, holding the lock from reading to writing. It returns the short
// ids also when saving fails.
func saveShortIDs(path string, refs []shortIDEntry) (shortIDs, error) {
	unlock, err := lockShortIDs(path)
	if err != nil {
		file := loadShortIDs(path)
		return assignShortIDs(refs, file.Entries, file.Forgotten), err
	}
	defer unlock()
	file := loadShortIDs(path)
	ids := assignShortIDs(refs, file.Entries, file.Forgotten)
	data, err := json.Marshal(mergeShortIDs(file, refs, ids))
	if err != nil {
		return ids, err
	}
	return ids, writeStateFile(path, data)
}

// mergeShortIDs puts refs at the top of the cache. Entries past
// maxShortIDs move to the forgotten list, and a message shown again comes
// off it.
func mergeShortIDs(file shortIDFile, refs []shortIDEntry, ids shortIDs) shortIDFile {
	seen := make(map[string]bool, len(refs)+len(file.Entries))
	merged := make([]shortIDEntry, 0, len(refs)+len(file.Entries))
	for _, ref := range refs {
		if ref.ID == "" || seen[ref.key()] {
			continue
		}
		seen[ref.key()] = true
		ref.Short = ids[ref.key()]
		merged = append(merged, ref)
	}
	for _, old := range file.Entries {
		if !seen[old.key()] {
			seen[old.key()] = true
			merged = append(merged, old)
		}
	}
	forgotten := append([]string(nil), file.Forgotten...)
	if len(merged) > maxShortIDs {
		for _, old := range merged[maxShortIDs:] {
			forgotten = append(forgotten, forgottenHash(old.Account, old.ID))
		}
		merged = merged[:maxShortIDs]
	}
	live := make(map[string]bool, len(merged))
	for _, e := range merged {
		live[forgottenHash(e.Account, e.ID)] = true
	}
	kept := make(map[string]bool, len(forgotten))
	out := shortIDFile{Version: shortIDVersion, Entries: merged}
	for _, h := range forgotten {
		if live[h] || kept[h] {
			continue
		}
		kept[h] = true
		out.Forgotten = append(out.Forgotten, h)
	}
	return out
}

// lockShortIDs takes the lock that lets one command at a time change the
// cache. It waits up to shortIDLockWait for another command to finish.
func lockShortIDs(path string) (func(), error) {
	unlock, err := fsutil.Lock(filepath.Join(filepath.Dir(path), shortIDsLockFile), shortIDLockWait)
	if errors.Is(err, fsutil.ErrBusy) {
		return nil, errors.New("another maildo command is still writing them")
	}
	return unlock, err
}

// messageRef is a message the cache should remember.
func messageRef(account, id string) shortIDEntry {
	return shortIDEntry{Short: shortID(account, id), Account: account, ID: id}
}

// messageRefWithSubject is messageRef with the subject, which helps people
// pick when a short id is ambiguous.
func messageRefWithSubject(account, id, subject string) shortIDEntry {
	ref := messageRef(account, id)
	ref.Subject = truncate(strings.TrimSpace(subject), 60)
	return ref
}

// lookupShortID finds the message a short id stands for. Two or more
// messages that start with it make it ambiguous: maildo refuses and lists
// them with longer ids. A short id that could have been printed for a
// message maildo has forgotten is refused too.
func lookupShortID(f *rungrad.Factory, short string) (shortIDEntry, error) {
	file := loadShortIDs(shortIDPath(f))
	entries := file.Entries
	live := make(map[string]bool, len(entries))
	for _, entry := range entries {
		live[forgottenHash(entry.Account, entry.ID)] = true
	}
	for _, gone := range file.Forgotten {
		if !live[gone] && strings.HasPrefix(gone, short) {
			return shortIDEntry{}, errs.NewWithDetails(errs.CodeInvalidSelector,
				fmt.Sprintf("maildo no longer remembers which message short id %s stood for", short),
				map[string]any{
					"short_id": short,
					"reason":   "forgotten",
					"hint":     "run maildo list or maildo search again and use the new short id, or use the full id from --json",
				})
		}
	}
	var matches []shortIDEntry
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.key()] || !strings.HasPrefix(idHash(entry.Account, entry.ID), short) {
			continue
		}
		seen[entry.key()] = true
		matches = append(matches, entry)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return shortIDEntry{}, errs.NewWithDetails(errs.CodeInvalidSelector,
			fmt.Sprintf("unknown short id %s", short),
			map[string]any{
				"short_id": short,
				"hint":     "short ids come from tables maildo printed on this computer; run maildo list or maildo search again, or use the full id from --json",
			})
	}
	longer := assignShortIDs(matches, entries, file.Forgotten)
	var names, candidates []string
	for _, m := range matches {
		id := longer.of(m.Account, m.ID)
		label := id + " in " + m.Account
		if m.Subject != "" {
			label += fmt.Sprintf(" (%q)", m.Subject)
		}
		names = append(names, label)
		candidates = append(candidates, id)
	}
	return shortIDEntry{}, errs.NewWithDetails(errs.CodeInvalidSelector,
		fmt.Sprintf("short id %s matches %d messages: %s", short, len(matches), strings.Join(names, ", ")),
		map[string]any{
			"short_id":   short,
			"candidates": candidates,
			"hint":       "use one of the longer ids, or the full id from --json",
		})
}

// expandShortIDs swaps short ids in --id flags for full ids before the
// command runs. When the user did not pick an account, it selects the
// account the short id belongs to.
func expandShortIDs(f *rungrad.Factory, cmd *cobra.Command) error {
	for _, name := range []string{"id", "draft-id"} {
		if err := expandShortIDFlag(f, cmd, name); err != nil {
			return err
		}
	}
	return nil
}

func expandShortIDFlag(f *rungrad.Factory, cmd *cobra.Command, name string) error {
	flag := cmd.Flags().Lookup(name)
	if flag == nil || !flag.Changed {
		return nil
	}
	var values []string
	array := flag.Value.Type() == "stringArray"
	if array {
		values, _ = cmd.Flags().GetStringArray(name)
	} else {
		v, _ := cmd.Flags().GetString(name)
		values = []string{v}
	}
	account := ""
	changed := false
	typed := append([]string(nil), values...)
	for i, raw := range values {
		v := strings.TrimSpace(raw)
		if !isShortID(v) {
			continue
		}
		entry, err := lookupShortID(f, v)
		if err != nil {
			return err
		}
		if account != "" && entry.Account != account {
			return errs.New(errs.CodeInvalidSelector, "these short ids belong to different accounts; run the command once for each account")
		}
		account = entry.Account
		values[i] = entry.ID
		changed = true
	}
	if !changed {
		return nil
	}
	// Keep what the user typed, for commands maildo prints back.
	if name == "id" {
		setAnnotation(cmd, annotationShortIDs, strings.Join(typed, "\x00"))
	}
	if array {
		if err := setStringArray(cmd, name, values); err != nil {
			return err
		}
	} else if err := cmd.Flags().Set(name, values[0]); err != nil {
		return err
	}
	return selectShortIDAccount(cmd, account)
}

// selectShortIDAccount points the command at the short id's account, unless
// the user named one. A different named account is an error.
func selectShortIDAccount(cmd *cobra.Command, account string) error {
	flag := cmd.Flags().Lookup("account")
	if flag == nil {
		return nil
	}
	if flag.Changed {
		named, _ := cmd.Flags().GetStringArray("account")
		for _, name := range named {
			if name == account {
				return nil
			}
		}
		return errs.NewWithDetails(errs.CodeInvalidSelector,
			fmt.Sprintf("the short id belongs to account %q, not %s", account, strings.Join(quoteAll(named), " or ")),
			map[string]any{"account": account})
	}
	if all := cmd.Flags().Lookup("all-accounts"); all != nil && all.Changed {
		return nil
	}
	setAnnotation(cmd, annotationShortAccount, "set")
	return cmd.Flags().Set("account", account)
}

const (
	annotationShortIDs     = "maildo.short_ids"
	annotationShortAccount = "maildo.short_account"
)

func setAnnotation(cmd *cobra.Command, key, value string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[key] = value
}

// clearShortIDAnnotations forgets the previous run's short ids.
func clearShortIDAnnotations(cmd *cobra.Command) {
	delete(cmd.Annotations, annotationShortIDs)
	delete(cmd.Annotations, annotationShortAccount)
}

// setStringArray replaces all values of a repeatable flag.
func setStringArray(cmd *cobra.Command, name string, values []string) error {
	flag := cmd.Flags().Lookup(name)
	type replacer interface{ Replace([]string) error }
	if r, ok := flag.Value.(replacer); ok {
		if err := r.Replace(values); err != nil {
			return err
		}
		flag.Changed = true
		return nil
	}
	return fmt.Errorf("flag --%s cannot be replaced", name)
}

func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}

// expandShortIDList swaps short ids in an ids file for full ids, like
// expandShortIDs does for --id.
func expandShortIDList(f *rungrad.Factory, cmd *cobra.Command, ids []string) ([]string, error) {
	out := make([]string, len(ids))
	account := ""
	for i, id := range ids {
		out[i] = id
		if !isShortID(id) {
			continue
		}
		entry, err := lookupShortID(f, id)
		if err != nil {
			return nil, err
		}
		if account != "" && entry.Account != account {
			return nil, errs.New(errs.CodeInvalidSelector, "these short ids belong to different accounts; put one account per file, or use account<TAB>id rows")
		}
		account = entry.Account
		out[i] = entry.ID
	}
	if account == "" {
		return out, nil
	}
	return out, selectShortIDAccount(cmd, account)
}

// writeStateFile replaces one of maildo's own files in the config folder
// (the short id cache, the record of sent approvals) through a temporary
// file and a rename, so readers never see half a file.
func writeStateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+strings.TrimSuffix(filepath.Base(path), ".json")+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
