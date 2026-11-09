# maildo forward

Forward a message (saves a draft unless you add --send)

## Usage

```
maildo forward [id]
```

## Examples

```
maildo forward 9eac029 --to anna@example.com --body "FYI"
maildo forward --id 3c1f2a7 --account work --to client@example.com --body-file note.md --markdown
maildo forward --query "from:client@example.com subject:quote" --latest --to client@example.com --body "FYI" --send --dry-run
maildo forward 9eac029 --to anna@example.com --body "FYI" --include-attachments none
maildo forward --id 3c1f2a7 --body "Preview" --dry-run --json
```

## Flags

- `--account` Account to use (default: the active account)
- `--approve` Approval code from the --dry-run preview; maildo sends only if the message still matches it, and only once
- `--bcc` Bcc recipient; repeat the flag or separate addresses with commas
- `--body` Message body as text
- `--body-file` Read the message body from a file, or from stdin with -
- `--cc` Cc recipient; repeat the flag or separate addresses with commas
- `--draft` Only save a draft; forward does this anyway unless you add --send
- `--first` With --query, use the first match; matches come newest first, so this is the same as --latest
- `--full` Show the whole body in the preview, also when it is long
- `--html` Write the body in HTML; maildo removes unsafe parts
- `--id` Message id, or a short id from list or search
- `--include-attachment` Name of an attachment of the original to send along (repeatable)
- `--include-attachment-id` Id of an attachment of the original to send along (repeatable)
- `--include-attachments` Which attachments of the original go along: all or none
- `--internet-id` Message-ID header of the message, such as <id@example.com>
- `--latest` With --query, use the newest match
- `--markdown` Write the body in Markdown; maildo turns it into HTML
- `--query` Search for the message; use with --latest or --first
- `--send` Send the forward instead of saving a draft
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

- reply
- draft update
- send
- read
