# maildo draft update

Change a draft

## Usage

```
maildo draft update [id]
```

## Examples

```
maildo draft update --id 3c1f2a7 --subject "Updated subject"
maildo draft update --id 3c1f2a7 --account work --body-file draft.html --html
maildo draft update --id 3c1f2a7 --clear-cc --json
maildo draft update --id 3c1f2a7 --body "Preview" --dry-run
```

## Flags

- `--account` Account to use (default: the active account)
- `--bcc` Bcc recipient; repeat the flag or separate addresses with commas
- `--body` Message body as text
- `--body-file` Read the message body from a file, or from stdin with -
- `--cc` Cc recipient; repeat the flag or separate addresses with commas
- `--clear-bcc` Remove all Bcc recipients
- `--clear-cc` Remove all Cc recipients
- `--clear-reply-to` Remove all Reply-To addresses
- `--clear-to` Remove all To recipients
- `--from` Sender, such as "Ann <ann@example.com>"; the address must be the account's own
- `--html` Write the body in HTML; maildo removes unsafe parts
- `--id` Draft id, or the short id maildo printed
- `--markdown` Write the body in Markdown; maildo turns it into HTML
- `--reply-to` Reply-To address; repeat the flag or separate addresses with commas
- `--subject` Message subject
- `--text` Write the body as plain text
- `--to` To recipient; repeat the flag or separate addresses with commas

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

- draft read
- draft send
- draft create
