package provider

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincentsch/maildo/internal/errs"
	"github.com/vincentsch/maildo/internal/mail"
)

type scopedFolderClient struct {
	account     string
	provider    string
	listStarted chan struct{}
	releaseList chan struct{}
	destroyed   chan struct{}
	destroyOnce atomic.Bool
}

func (c *scopedFolderClient) ProviderName() string { return c.provider }
func (c *scopedFolderClient) AccountName() string  { return c.account }
func (c *scopedFolderClient) Destroy() {
	if c.destroyOnce.CompareAndSwap(false, true) && c.destroyed != nil {
		close(c.destroyed)
	}
}

func (c *scopedFolderClient) ListFolders(context.Context, mail.FolderListRequest) (mail.FolderList, error) {
	if c.listStarted != nil {
		close(c.listStarted)
	}
	if c.releaseList != nil {
		<-c.releaseList
	}
	return mail.FolderList{}, nil
}

func TestAccountClientScopeWaitsForOpeningClientBeforeDestroy(t *testing.T) {
	ctx := context.Background()
	openStarted := make(chan struct{})
	releaseOpen := make(chan struct{})
	destroyed := make(chan struct{})
	clientErr := make(chan error, 1)
	callbackCalled := atomic.Bool{}

	scopeDone := make(chan error, 1)
	go func() {
		scopeDone <- WithAccountClientScope(ctx, func(scope AccountClientScope) error {
			acct := scopedAccountForTest(scope.(*commandAccountClientScope), func(context.Context) (AccountClient, error) {
				close(openStarted)
				<-releaseOpen
				return &scopedFolderClient{account: "work", provider: "fake", destroyed: destroyed}, nil
			})
			go func() {
				clientErr <- WithAccountClient[FolderReader](ctx, scope, acct, ClientAccessFoldersRead, func(reader FolderReader) error {
					callbackCalled.Store(true)
					_, err := reader.ListFolders(ctx, mail.FolderListRequest{})
					return err
				})
			}()
			<-openStarted
			return nil
		})
	}()

	assertNotReady(t, scopeDone, "scope returned while client was still opening")
	close(releaseOpen)
	if err := <-scopeDone; err != nil {
		t.Fatalf("scope error: %v", err)
	}
	if callbackCalled.Load() {
		t.Fatal("client callback ran after scope close began")
	}
	assertClosed(t, destroyed, "opened client was not destroyed")
	assertProviderCode(t, <-clientErr, errs.CodeProviderClientContractInvalid)
}

func TestAccountClientScopeWaitsForRunningCallbackBeforeDestroy(t *testing.T) {
	ctx := context.Background()
	listStarted := make(chan struct{})
	releaseList := make(chan struct{})
	destroyed := make(chan struct{})
	clientErr := make(chan error, 1)

	scopeDone := make(chan error, 1)
	go func() {
		scopeDone <- WithAccountClientScope(ctx, func(scope AccountClientScope) error {
			acct := scopedAccountForTest(scope.(*commandAccountClientScope), func(context.Context) (AccountClient, error) {
				return &scopedFolderClient{
					account:     "work",
					provider:    "fake",
					listStarted: listStarted,
					releaseList: releaseList,
					destroyed:   destroyed,
				}, nil
			})
			go func() {
				clientErr <- WithAccountClient[FolderReader](ctx, scope, acct, ClientAccessFoldersRead, func(reader FolderReader) error {
					_, err := reader.ListFolders(ctx, mail.FolderListRequest{})
					return err
				})
			}()
			<-listStarted
			return nil
		})
	}()

	assertNotReady(t, scopeDone, "scope returned while loan callback was running")
	assertNotReady(t, destroyed, "client was destroyed while loan callback was running")
	close(releaseList)
	if err := <-clientErr; err != nil {
		t.Fatalf("loan callback error: %v", err)
	}
	if err := <-scopeDone; err != nil {
		t.Fatalf("scope error: %v", err)
	}
	assertClosed(t, destroyed, "client was not destroyed after loan callback finished")
}

func TestValidateCapabilityTableRequiresCompleteKnownStates(t *testing.T) {
	caps := implementedCaps()
	delete(caps, CapMessagesRead)
	assertProviderCode(t, validateCapabilityTableError(caps), errs.CodeSharedProviderContractInvalid)

	caps = implementedCaps()
	caps[CapMessagesRead] = CapUnknown
	assertProviderCode(t, validateCapabilityTableError(caps), errs.CodeSharedProviderContractInvalid)

	caps = implementedCaps()
	caps[Capability("provider_private")] = CapImplemented
	assertProviderCode(t, validateCapabilityTableError(caps), errs.CodeSharedProviderContractInvalid)

	if _, err := validateCapabilityTable(implementedCaps()); err != nil {
		t.Fatalf("complete capability table rejected: %v", err)
	}
}

func TestComposeSourceSnapshotTransferConsumesTheOriginal(t *testing.T) {
	scope := &commandAccountClientScope{}
	material := ComposeSourceMaterial{SourceID: "src1", ThreadID: "thread1", Text: []byte("source")}
	source, err := newComposeSourceSnapshot("work", "fake", scope, material)
	if err != nil {
		t.Fatal(err)
	}
	transferred, err := source.Transfer()
	if err != nil {
		t.Fatal(err)
	}
	defer transferred.Destroy()
	if _, err := source.Clone(); err == nil {
		t.Fatal("transferred source cloned after consumption")
	}
	var text string
	if err := transferred.Use(func(m ComposeSourceMaterial) error { text = string(m.Text); return nil }); err != nil || text != "source" {
		t.Fatalf("transferred material = %q, %v", text, err)
	}
}

func TestForwardAttachmentAccessNamesMatchContract(t *testing.T) {
	if ClientAccessForwardDraftCreateWithAttachments != "forward_draft_create_with_attachments" {
		t.Fatalf("create-with-attachments access = %q", ClientAccessForwardDraftCreateWithAttachments)
	}
	if ClientAccessForwardDraftSendWithAttachments != "forward_draft_send_with_attachments" {
		t.Fatalf("send-with-attachments access = %q", ClientAccessForwardDraftSendWithAttachments)
	}
	if ClientAccessForwardDraftCreatePreviewWithAttachments != "forward_draft_create_preview_with_attachments" {
		t.Fatalf("create-preview-with-attachments access = %q", ClientAccessForwardDraftCreatePreviewWithAttachments)
	}
	if ClientAccessForwardDraftSendPreviewWithAttachments != "forward_draft_send_preview_with_attachments" {
		t.Fatalf("send-preview-with-attachments access = %q", ClientAccessForwardDraftSendPreviewWithAttachments)
	}
	if got, want := accessCapabilities[ClientAccessDraftAttachmentsMutate], []Capability{CapDraftRead, CapAttachmentsMutate}; !slices.Equal(got, want) {
		t.Fatalf("draft attachment mutate caps = %#v, want %#v", got, want)
	}
	if got, want := accessCapabilities[ClientAccessForwardDraftCreatePreviewWithAttachments], []Capability{CapForwardDraftCreate, CapAttachmentsRead, CapAttachmentsMutate}; !slices.Equal(got, want) {
		t.Fatalf("forward create preview caps = %#v, want %#v", got, want)
	}
	if got, want := accessCapabilities[ClientAccessForwardDraftSendPreviewWithAttachments], []Capability{CapForwardDraftCreate, CapDraftSend, CapAttachmentsRead, CapAttachmentsMutate}; !slices.Equal(got, want) {
		t.Fatalf("forward send preview caps = %#v, want %#v", got, want)
	}
}

func TestRunBoundedOrderedSuccessAndDeterministicFailure(t *testing.T) {
	got, err := RunBoundedOrdered(context.Background(), 2, []int{1, 2, 3}, func(_ context.Context, ordinal int, in int) (int, error) {
		return in*10 + ordinal, nil
	})
	if err != nil || len(got) != 3 || got[0] != 10 || got[1] != 21 || got[2] != 32 {
		t.Fatalf("success = %#v, %v", got, err)
	}

	errHigh := errors.New("high ordinal")
	errLow := errors.New("low ordinal")
	_, err = RunBoundedOrdered(context.Background(), 3, []int{0, 1, 2}, func(_ context.Context, ordinal int, _ int) (int, error) {
		if ordinal == 2 {
			return 0, errHigh
		}
		if ordinal == 1 {
			time.Sleep(10 * time.Millisecond)
			return 0, errLow
		}
		time.Sleep(20 * time.Millisecond)
		return ordinal, nil
	})
	if !errors.Is(err, errLow) {
		t.Fatalf("error = %v, want lowest ordinal intrinsic failure", err)
	}
}

func TestRunBoundedOrderedCoordinatorCancellationDoesNotMaskIntrinsicFailure(t *testing.T) {
	intrinsic := errors.New("intrinsic")
	_, err := RunBoundedOrdered(context.Background(), 2, []int{0, 1}, func(ctx context.Context, ordinal int, _ int) (int, error) {
		if ordinal == 0 {
			return 0, intrinsic
		}
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if !errors.Is(err, intrinsic) {
		t.Fatalf("error = %v, want intrinsic failure", err)
	}
}

func TestRunBoundedOrderedLowestOrdinalPanicWins(t *testing.T) {
	defer func() {
		if p := recover(); p != "low" {
			t.Fatalf("panic = %#v, want low", p)
		}
	}()
	_, _ = RunBoundedOrdered(context.Background(), 2, []int{0, 1}, func(_ context.Context, ordinal int, _ int) (int, error) {
		if ordinal == 0 {
			time.Sleep(10 * time.Millisecond)
			panic("low")
		}
		panic("high")
	})
}

func scopedAccountForTest(scope *commandAccountClientScope, open AccountClientOpenFunc) ResolvedAccount {
	return ResolvedAccount{
		Name:        "work",
		Provider:    "fake",
		Email:       "work@example.test",
		DisplayName: "Work Account",
		token: &resolvedAccountToken{
			scope:    scope,
			provider: "fake",
			account:  "work",
			email:    "work@example.test",
			display:  "Work Account",
			open:     open,
			caps:     implementedCaps(),
		},
	}
}

func validateCapabilityTableError(caps Capabilities) error {
	_, err := validateCapabilityTable(caps)
	return err
}

func assertNotReady[T any](t *testing.T, ch <-chan T, message string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(message)
	case <-time.After(25 * time.Millisecond):
	}
}

func assertClosed(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func assertProviderCode(t *testing.T, err error, code string) {
	t.Helper()
	var product *errs.Error
	if !errors.As(err, &product) || product.Code() != code {
		t.Fatalf("error = %#v, want product code %q", err, code)
	}
}
