package mail

import (
	"encoding/json"
	"testing"
)

func TestAttachmentJSONIncludesExplicitSizeFactsWhenPresent(t *testing.T) {
	body, err := json.Marshal(Attachment{
		ID:          "part-1",
		Name:        "empty.txt",
		ContentType: "text/plain",
		Kind:        AttachmentKindFile,
		SizeFacts: AttachmentSizeFacts{
			EncodedSize:      0,
			EncodedSizeKnown: false,
			DecodedSize:      0,
			DecodedSizeKnown: true,
		},
		SizeFactsPresent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"encoded_size", "encoded_size_known", "decoded_size", "decoded_size_known"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %s in %s", key, body)
		}
	}
	if got["encoded_size_known"] != false || got["decoded_size_known"] != true || got["decoded_size"] != float64(0) {
		t.Fatalf("size facts = %#v", got)
	}

	body, err = json.Marshal(Attachment{ID: "outlook-style", Kind: AttachmentKindFile})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"encoded_size", "encoded_size_known", "decoded_size", "decoded_size_known"} {
		if _, ok := got[key]; ok {
			t.Fatalf("unexpected %s in %s", key, body)
		}
	}
}
