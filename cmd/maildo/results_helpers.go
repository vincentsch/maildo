package main

import "github.com/vincentsch/maildo/internal/mail"

func cloneBoolPtr(in *bool) *bool {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneProviderMetadataForResult[T mail.ProviderMetadataValue](in *T) *T {
	out, err := mail.CloneProviderMetadata(in)
	if err != nil {
		panic(err)
	}
	return out
}
