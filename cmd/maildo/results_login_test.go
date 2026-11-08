package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoginTellsHowToAddANameWhenFromHasNone(t *testing.T) {
	var out bytes.Buffer
	humanLogin(loginResult{Account: "personal", Provider: "gmail", Email: "dana@example.test", Active: true, unnamed: true})(&out)
	if !strings.Contains(out.String(), `maildo config set sender_name "Your Name" --account personal`) {
		t.Fatalf("output = %q", out.String())
	}
	out.Reset()
	humanLogin(loginResult{Account: "work", Provider: "outlook", Email: "dana@example.test", DisplayName: "Dana", Active: true})(&out)
	if strings.Contains(out.String(), "sender_name") {
		t.Fatalf("output = %q", out.String())
	}
}
