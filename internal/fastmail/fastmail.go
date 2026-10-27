// Package fastmail is the IMAP and SMTP provider with Fastmail's servers
// filled in.
package fastmail

import "github.com/vincentsch/maildo/internal/imap"

func New(opts imap.Options) *imap.Provider {
	return imap.NewProvider("fastmail", opts)
}

func CredentialCodec() imap.CredentialCodec {
	return imap.NewCredentialCodec("fastmail")
}
