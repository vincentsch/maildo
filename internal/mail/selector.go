package mail

import (
	"strings"

	"github.com/vincentsch/maildo/internal/errs"
)

var allowedBodyModes = []string{"readable", "text", "html", "raw"}

// ParseBodyMode checks a --body-mode value. Empty means readable.
func ParseBodyMode(s string) (BodyMode, error) {
	switch strings.TrimSpace(s) {
	case "":
		return BodyModeReadable, nil
	case string(BodyModeReadable):
		return BodyModeReadable, nil
	case string(BodyModeText):
		return BodyModeText, nil
	case string(BodyModeHTML):
		return BodyModeHTML, nil
	case string(BodyModeRaw):
		return BodyModeRaw, nil
	default:
		return "", errs.NewWithDetails(errs.CodeInvalidSelector, "unknown body mode; use readable, text, html, or raw", map[string]any{
			"requested_body_mode": s,
			"allowed":             allowedBodyModes,
		})
	}
}

// SelectorFlags holds the flags that pick a message, as ParseMessageSelector
// reads them.
type SelectorFlags struct {
	ID            string
	HasID         bool
	InternetID    string
	HasInternetID bool
	ThreadID      string
	HasThreadID   bool
	Query         *Query
	HasQuery      bool
	Latest        bool
	First         bool
	AllowThreadID bool
}

// ParseMessageSelector checks that the flags pick a message in exactly one
// way and returns that way.
func ParseMessageSelector(in SelectorFlags) (MessageSelector, error) {
	if in.HasThreadID && !in.AllowThreadID {
		return MessageSelector{}, invalidSelector("this command does not take --thread-id")
	}
	if in.HasID && in.ID == "" {
		return MessageSelector{}, invalidSelector("--id requires a value")
	}
	if in.HasInternetID && in.InternetID == "" {
		return MessageSelector{}, invalidSelector("--internet-id requires a value")
	}
	if in.HasThreadID && in.ThreadID == "" {
		return MessageSelector{}, invalidSelector("--thread-id requires a value")
	}

	families := 0
	if in.HasID {
		families++
	}
	if in.HasInternetID {
		families++
	}
	if in.HasThreadID && in.AllowThreadID {
		families++
	}
	if in.HasQuery {
		families++
	}
	if families == 0 {
		return MessageSelector{}, invalidSelector("say which message: give its id, or use --internet-id or --query")
	}
	if families > 1 {
		return MessageSelector{}, invalidSelector("pick the message in one way only: with an id, --internet-id or --query")
	}

	if !in.HasQuery && (in.Latest || in.First) {
		return MessageSelector{}, invalidSelector("--latest and --first only work with --query")
	}
	if in.HasQuery {
		if in.Query == nil {
			return MessageSelector{}, invalidSelector("--query requires a parsed query")
		}
		if in.Latest == in.First {
			if in.Latest {
				return MessageSelector{}, invalidSelector("--latest and --first cannot be used together")
			}
			return MessageSelector{}, invalidSelector("--query needs --latest or --first to pick one message")
		}
		// Both picks take the first row of the newest-first results today.
		// The selector still records which one the user typed, in case they
		// ever mean different things.
		pick := PickFirst
		if in.Latest {
			pick = PickLatest
		}
		return MessageSelector{Kind: SelectorQuery, Query: in.Query, Pick: pick}, nil
	}

	switch {
	case in.HasID:
		return MessageSelector{Kind: SelectorID, ID: in.ID}, nil
	case in.HasInternetID:
		return MessageSelector{Kind: SelectorInternetID, InternetID: in.InternetID}, nil
	case in.HasThreadID:
		return MessageSelector{Kind: SelectorThreadID, ThreadID: in.ThreadID}, nil
	default:
		return MessageSelector{}, invalidSelector("unsupported selector")
	}
}

// Resolved turns a selector that names a message directly into the form the
// providers take. Query selectors are resolved by searching first.
func (s MessageSelector) Resolved() ResolvedMessageSelector {
	switch s.Kind {
	case SelectorID:
		return ResolvedMessageSelector{Kind: ResolvedSelectorID, ID: s.ID}
	case SelectorInternetID:
		return ResolvedMessageSelector{Kind: ResolvedSelectorInternetID, InternetID: s.InternetID}
	case SelectorThreadID:
		return ResolvedMessageSelector{Kind: ResolvedSelectorThreadID, ThreadID: s.ThreadID}
	default:
		panic("mail: query selector cannot be resolved without search")
	}
}

func invalidSelector(message string) error {
	return errs.New(errs.CodeInvalidSelector, message)
}
