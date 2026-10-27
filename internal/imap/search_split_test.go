package imap

import (
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/vincentsch/maildo/internal/mail"
)

func TestSplitAddressWordsForServersThatRefuseSpaces(t *testing.T) {
	criteria, _ := searchCriteria(mail.Query{From: []string{"Clara Client", "bob"}, Subject: []string{"Project kickoff"}})
	split, changed := splitAddressWords(criteria)
	if !changed {
		t.Fatal("nothing was split")
	}
	var keys []string
	var walk func(c goimap.SearchCriteria)
	walk = func(c goimap.SearchCriteria) {
		for _, kv := range c.Header {
			keys = append(keys, kv.Key+"="+kv.Value)
		}
		for _, pair := range c.Or {
			walk(pair[0])
			walk(pair[1])
		}
	}
	walk(split)
	want := map[string]bool{"From=Clara": true, "From=Client": true, "From=bob": true, "Subject=Project kickoff": true}
	for _, k := range keys {
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("keys = %v, missing %v", keys, want)
	}
}
