package outlook

import (
	"context"
	"strings"

	"github.com/spf13/cobra"
	maildoconfig "github.com/vincentsch/maildo/internal/config"
	"github.com/vincentsch/maildo/internal/provider"
)

type SetupParser struct{}

func (SetupParser) ConfigureFlags(cmd *cobra.Command) error {
	cmd.Flags().Bool("device-code", false, "Sign in with a code on another device, for machines without a browser")
	return nil
}

func (SetupParser) Resolve(_ context.Context, ctx provider.SetupResolveContext) (provider.SetupResolution, error) {
	setup := maildoconfig.ProviderSetup{Outlook: &maildoconfig.OutlookSetup{}}
	source := "none"
	if ctx.Existing.Outlook != nil && strings.TrimSpace(ctx.Existing.Outlook.ClientID) != "" {
		setup.Outlook.ClientID = strings.TrimSpace(ctx.Existing.Outlook.ClientID)
		source = "config"
	}
	if ctx.LookupEnv != nil {
		if value, ok, err := ctx.LookupEnv(envClientID); err != nil {
			return provider.SetupResolution{}, err
		} else if ok && strings.TrimSpace(value) != "" {
			setup.Outlook.ClientID = strings.TrimSpace(value)
			source = "environment"
		}
	}
	flow := provider.InteractiveAuthFlowDefault
	if flag, ok := ctx.Flags.Lookup("device-code"); ok && flag.Changed && strings.EqualFold(flag.Value, "true") {
		flow = provider.InteractiveAuthFlowDeviceCode
	}
	configured := "false"
	if setup.Outlook.ClientID != "" {
		configured = "true"
	}
	return provider.SetupResolution{
		Setup: setup,
		Flow:  flow,
		Preview: []provider.SetupPreviewField{
			{Name: "client_id_configured", Value: configured},
			{Name: "client_id_source", Value: source},
		},
	}, nil
}

func (SetupParser) PromptMissing(_ context.Context, _ provider.SetupPromptContext, resolution provider.SetupResolution) (provider.SetupResolution, error) {
	return resolution, nil
}

var _ provider.SetupParser = SetupParser{}
