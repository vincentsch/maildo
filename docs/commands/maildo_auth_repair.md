# maildo auth repair

Log in again when a saved login stopped working

## Usage

```
maildo auth repair <name>
```

## Examples

```
maildo auth repair work
maildo auth repair personal --gmail-client-id CLIENT_ID
maildo auth repair hosted --imap-host imap.example.net --imap-port 993 --imap-tls implicit --smtp-host smtp.example.net --smtp-port 465 --smtp-tls implicit
maildo auth repair work --device-code
maildo auth repair work --dry-run
maildo auth repair work --json
```

## Flags

- `--ca-file` Certificate authority (PEM file) to trust in addition to the system's
- `--device-code` Sign in with a code on another device, for machines without a browser
- `--email` Your email address; maildo looks up the servers from it
- `--gmail-client-id` Client ID of your own Google OAuth app (type Desktop app)
- `--imap-host` IMAP server name, such as imap.example.com
- `--imap-port` IMAP server port (default 993, or 143 with --imap-tls starttls)
- `--imap-tls` IMAP TLS mode: implicit or starttls (default: from the port)
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

- auth refresh
- auth status
