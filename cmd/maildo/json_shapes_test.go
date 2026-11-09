package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The JSON of list, search, read, thread and draft read uses one style:
// the flags are read, flagged and has_attachments, and every list of
// addresses is there, as [] when it is empty.
func TestMessageJSONHasOneStyleAndNoNullLists(t *testing.T) {
	h := newSendHarness(t)
	h.server.Append("INBOX", "From: Maria <maria@example.org>\r\nTo: me@posteo.de\r\nSubject: Logo files\r\nMessage-ID: <logo@example.org>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nHere.\r\n--b\r\nContent-Type: image/png\r\nContent-Disposition: inline; filename=logo.png\r\n\r\nPNG\r\n--b--\r\n", time.Now())
	id := searchOne(t, h, "subject:logo", "INBOX")

	var list struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	decodeOK(t, h.run("list", "--json"), &list)
	row := list.Messages[0]
	for _, key := range []string{"read", "flagged", "has_attachments", "to_count", "cc_count"} {
		if _, ok := row[key]; !ok {
			t.Errorf("list row has no %q: %v", key, keysOf(row))
		}
	}
	if _, ok := row["is_read"]; ok {
		t.Error("list row still has is_read")
	}

	var read struct {
		Message map[string]json.RawMessage `json:"message"`
	}
	decodeOK(t, h.run("read", id, "--json"), &read)
	for _, key := range []string{"cc", "bcc", "reply_to"} {
		if got := string(read.Message[key]); got != "[]" {
			t.Errorf("read: %s = %s, want []", key, got)
		}
	}
	if string(read.Message["read"]) != "false" || string(read.Message["flagged"]) != "false" {
		t.Errorf("read: read = %s, flagged = %s", read.Message["read"], read.Message["flagged"])
	}
	var attachments []map[string]any
	if err := json.Unmarshal(read.Message["attachments"], &attachments); err != nil || len(attachments) != 1 || attachments[0]["inline"] != true {
		t.Errorf("read: attachments = %s", read.Message["attachments"])
	}

	var thread struct {
		Thread struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		} `json:"thread"`
	}
	decodeOK(t, h.run("thread", id, "--json"), &thread)
	msg := thread.Thread.Messages[0]
	if !strings.Contains(string(msg["to"]), "me@posteo.de") || string(msg["cc"]) != "[]" || string(msg["flagged"]) != "false" || string(msg["read"]) != "false" {
		t.Errorf("thread message: to %s, cc %s, flagged %s, read %s", msg["to"], msg["cc"], msg["flagged"], msg["read"])
	}

	var saved draftWriteResult
	decodeOK(t, h.run("draft", "create", "--to", "jonas@example.org", "--subject", "Plan", "--body", "Soon.", "--json"), &saved)
	var draft struct {
		Draft map[string]json.RawMessage `json:"draft"`
	}
	decodeOK(t, h.run("draft", "read", saved.DraftID, "--json"), &draft)
	if string(draft.Draft["cc"]) != "[]" || string(draft.Draft["bcc"]) != "[]" || string(draft.Draft["reply_to"]) != "[]" || string(draft.Draft["draft"]) != "true" {
		t.Errorf("draft read: cc %s, bcc %s, reply_to %s, draft %s", draft.Draft["cc"], draft.Draft["bcc"], draft.Draft["reply_to"], draft.Draft["draft"])
	}
}

// new_id is the id a message has after a change. A change that keeps the
// id, such as a flag, repeats it instead of leaving new_id out.
func TestNewIDIsSetAfterEveryChange(t *testing.T) {
	h, _ := seededMutationHarness(t)
	for _, args := range [][]string{{"flag", "set", "--id", "work-new"}, {"flag", "set", "--id", "work-new"}, {"mark", "read", "--id", "work-new"}} {
		var result map[string]json.RawMessage
		decodeOK(t, h.run(append(args, "--json")...), &result)
		if string(result["new_id"]) != `"work-new"` {
			t.Fatalf("%s: new_id = %s", strings.Join(args, " "), result["new_id"])
		}
	}
	var bulk struct {
		Targets []map[string]json.RawMessage `json:"targets"`
	}
	decodeOK(t, h.run("flag", "clear", "--id", "work-new", "--id", "work-old", "--apply", "--json"), &bulk)
	if len(bulk.Targets) != 2 {
		t.Fatalf("targets = %v", bulk.Targets)
	}
	for _, row := range bulk.Targets {
		if len(row["new_id"]) == 0 || string(row["new_id"]) != string(row["id"]) {
			t.Errorf("row: id %s, new_id %s", row["id"], row["new_id"])
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
