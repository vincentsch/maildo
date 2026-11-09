# maildo send

Send a new message

## Usage

```
maildo send
```

## Examples

```
maildo send --to client@example.com --subject "Quote" --body "Message body"
maildo send --to client@example.com --subject "Quote" --body "Message body" --dry-run
maildo send --account work --to client@example.com --subject "Quote" --body "Message body" --approve k7q2m9xd --yes
maildo send --to client@example.com --subject "Quote" --body-file quote.txt --attach quote.pdf --dry-run --json
```

## Flags

- `--account` Account to use (default: the active account)
- `--approve` Approval code from the --dry-run preview; maildo sends only if the message still matches it, and only once
- `--attach` File to attach (repeatable)
- `--bcc` Bcc recipient; repeat the flag or separate addresses with commas
- `--body` Message body as text
- `--body-file` Read the message body from a file, or from stdin with -
- `--cc` Cc recipient; repeat the flag or separate addresses with commas
- `--from` Sender, such as "Ann <ann@example.com>"; the address must be the account's own
- `--full` Show the whole body in the preview, also when it is long
- `--html` Write the body in HTML; maildo removes unsafe parts
- `--markdown` Write the body in Markdown; maildo turns it into HTML
- `--reply-to` Reply-To address; repeat the flag or separate addresses with commas
- `--subject` Message subject
- `--text` Write the body as plain text
- `--to` To recipient; repeat the flag or separate addresses with commas
- `--yes` Send without asking; needs --approve unless the account allows unattended sends

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Preview changes

This command changes things. Add `--dry-run` to see what it would change; nothing changes then.

## Request details

Add `--include-meta` to the JSON output to get the result as `{data, meta}`, where `meta` holds details about the request.

## Related commands

- draft create
- draft send
