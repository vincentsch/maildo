# maildo login

Log in to a mail account

## Usage

```
maildo login
```

## Examples

```
maildo login
maildo login --email you@example.com
MAILDO_IMAP_PASSWORD=app-password maildo login --email you@posteo.de
maildo login --email you@example.net --imap-host imap.example.net --smtp-host smtp.example.net
maildo login --email you@posteo.de --dry-run
maildo login --provider fastmail --name personal --email you@fastmail.com
maildo login --provider outlook --name work
maildo login --provider outlook --name work --device-code
maildo login --provider gmail --name personal --gmail-client-id CLIENT_ID
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

This command changes things. Add `--dry-run` to see what it would change; nothing changes then.

## Request details

Add `--include-meta` to the JSON output to get the result as `{data, meta}`, where `meta` holds details about the request.

## Related commands

- auth login
- auth list
- auth status
