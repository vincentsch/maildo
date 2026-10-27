package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type noticeRuntime struct {
	notices []string
	opened  string
	openErr error
}

func (r *noticeRuntime) RegisterSecret(string) error                  { return nil }
func (r *noticeRuntime) LookupSecretEnv(string) (string, bool, error) { return "", false, nil }
func (r *noticeRuntime) WriteNotice(text string) error {
	r.notices = append(r.notices, text)
	return nil
}
func (r *noticeRuntime) OpenBrowser(_ context.Context, url string) error {
	r.opened = url
	return r.openErr
}

func TestOpenSignInPagePrintsTheAddressBeforeWaiting(t *testing.T) {
	r := &noticeRuntime{}
	if err := OpenSignInPage(context.Background(), r, "https://login.example/authorize?x=1", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.notices, "\n")
	want := "Opening your browser to sign in. If it does not open, visit this address:\nhttps://login.example/authorize?x=1\nWaiting for you to finish signing in (up to 5 minutes)..."
	if got != want || r.opened != "https://login.example/authorize?x=1" {
		t.Fatalf("notices:\n%s\nopened %q", got, r.opened)
	}
}

func TestOpenSignInPageKeepsWaitingWhenTheBrowserFails(t *testing.T) {
	r := &noticeRuntime{openErr: errors.New("xdg-open not found")}
	if err := OpenSignInPage(context.Background(), r, "https://login.example/a", 3*time.Minute); err != nil {
		t.Fatalf("a missing browser should not stop the sign-in: %v", err)
	}
	if got := strings.Join(r.notices, "\n"); !strings.Contains(got, "The browser did not open") || !strings.HasSuffix(got, "(up to 3 minutes)...") {
		t.Fatalf("notices:\n%s", got)
	}
	r = &noticeRuntime{openErr: ErrProviderRuntimeDenied}
	if err := OpenSignInPage(context.Background(), r, "https://login.example/a", time.Minute); !errors.Is(err, ErrProviderRuntimeDenied) {
		t.Fatalf("a denied runtime must stop the sign-in, got %v", err)
	}
}
