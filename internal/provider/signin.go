package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// OpenSignInPage prints the sign-in address, then tries to open it in a
// browser, and says how long maildo waits. The address comes first so the
// person can open it by hand when no browser starts, for example in WSL or
// in a coding agent's shell. That is why a browser that does not open is not
// an error. The browser has to run on this computer, because the sign-in
// ends at a local address that maildo listens on.
func OpenSignInPage(ctx context.Context, runtime InteractiveProviderRuntime, url string, wait time.Duration) error {
	if err := runtime.WriteNotice("Opening your browser to sign in. If it does not open, visit this address:"); err != nil {
		return err
	}
	if err := runtime.WriteNotice(url); err != nil {
		return err
	}
	if err := runtime.OpenBrowser(ctx, url); err != nil {
		if errors.Is(err, ErrProviderRuntimeClosed) || errors.Is(err, ErrProviderRuntimeDenied) {
			return err
		}
		if err := runtime.WriteNotice("The browser did not open. Open the address above in a browser on this computer."); err != nil {
			return err
		}
	}
	return runtime.WriteNotice(fmt.Sprintf("Waiting for you to finish signing in (up to %s)...", waitText(wait)))
}

func waitText(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		if d == time.Minute {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return d.Round(time.Second).String()
}
