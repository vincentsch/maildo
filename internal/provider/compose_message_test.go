package provider

import (
	"testing"

	"github.com/vincentsch/maildo/internal/mail"
)

func TestReplyRecipients(t *testing.T) {
	me := mail.Recipient{Name: "Dana", Address: "dana@example.test"}
	ann := mail.Recipient{Name: "Ann", Address: "ann@example.test"}
	bob := mail.Recipient{Name: "Bob", Address: "bob@example.test"}
	cases := []struct {
		name   string
		source ComposeSourceMaterial
		want   []string
	}{
		{"from someone else", ComposeSourceMaterial{From: ann, To: []mail.Recipient{me}}, []string{"ann@example.test"}},
		{"reply-to wins over from", ComposeSourceMaterial{From: ann, ReplyTo: []mail.Recipient{bob}, To: []mail.Recipient{me}}, []string{"bob@example.test"}},
		{"your own message goes to its recipients", ComposeSourceMaterial{From: me, To: []mail.Recipient{ann, bob}}, []string{"ann@example.test", "bob@example.test"}},
		{"a note to yourself", ComposeSourceMaterial{From: me, To: []mail.Recipient{me}}, []string{"dana@example.test"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := replyRecipients(tc.source, "dana@example.test")
			var addrs []string
			for _, r := range got {
				addrs = append(addrs, r.Address)
			}
			if len(addrs) != len(tc.want) {
				t.Fatalf("got %v, want %v", addrs, tc.want)
			}
			for i := range addrs {
				if addrs[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", addrs, tc.want)
				}
			}
		})
	}
}
