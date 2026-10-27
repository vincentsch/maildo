# maildo auth login

Log in to a mail account

## Usage

```
maildo auth login
```

## Examples

```
maildo auth login
maildo auth login --email you@example.com
MAILDO_IMAP_PASSWORD=app-password maildo auth login --email you@posteo.de
maildo auth login --email you@example.net --imap-host imap.example.net --smtp-host smtp.example.net
maildo auth login --provider fastmail --name personal --email you@fastmail.com
maildo auth login --provider outlook --name work
maildo auth login --provider outlook --name work --device-code
maildo auth login --provider gmail --name personal --gmail-client-id CLIENT_ID
```

## Flags

- `--ca-file` Certificate authority (PEM file) to trust in addition to the system's
- `--device-code` Sign in with a code on another device, for machines without a browser
- `--email` Your email address; maildo looks up the servers from it
- `--gmail-client-id` Client ID of your own Google OAuth app (type Desktop app)
- `--imap-host` IMAP server name, such as imap.example.com
- `--imap-port` IMAP server port (default 993, or 143 with --imap-tls starttls)
- `--imap-tls` IMAP TLS mode: implicit or starttls (default: from the port)
- `--name` Account name (default: suggested from the address)
- `--no-set-active` Leave the active account as it is
- `--provider` Provider: imap, fastmail, gmail or outlook (default: picked from --email)
- `--set-active` Make this the active account
- `--smtp-host` SMTP server name, such as smtp.example.com
- `--smtp-port` SMTP server port (default 465, or 587 with --smtp-tls starttls)
- `--smtp-tls` SMTP TLS mode: implicit or starttls (default: from the port)
- `--username` Username for the IMAP and SMTP servers (default: the email address)

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Preview changes

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- login
- auth list
- auth status
