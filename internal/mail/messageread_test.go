package mail

import (
	"encoding/json"
	"testing"
)

// JSON lists a size only when the provider knows it for sure. An estimate
// stays out, so encoded_size, decoded_size and size never seem to disagree.
func TestAttachmentJSONListsOnlyKnownSizes(t *testing.T) {
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
	if got["decoded_size"] != float64(0) {
		t.Fatalf("decoded_size = %#v in %s", got["decoded_size"], body)
	}
	for _, key := range []string{"encoded_size", "encoded_size_known", "decoded_size_known"} {
		if _, ok := got[key]; ok {
			t.Fatalf("unexpected %s in %s", key, body)
		}
	}

	// IMAP knows the encoded size and estimates the file size from it.
	body, err = json.Marshal(Attachment{ID: "imap-style", Kind: AttachmentKindFile, Size: 318,
		SizeFacts: AttachmentSizeFacts{EncodedSize: 436, EncodedSizeKnown: true, DecodedSize: 318}, SizeFactsPresent: true})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["decoded_size"]; ok || got["encoded_size"] != float64(436) || got["size"] != float64(318) {
		t.Fatalf("estimated sizes = %s", body)
	}

	body, err = json.Marshal(Attachment{ID: "outlook-style", Kind: AttachmentKindFile})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"encoded_size", "decoded_size"} {
		if _, ok := got[key]; ok {
			t.Fatalf("unexpected %s in %s", key, body)
		}
	}
}
