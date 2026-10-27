# maildo reply

Reply to a message (saves a draft unless you add --send)

## Usage

```
maildo reply [id]
```

## Examples

```
maildo reply 9eac029 --body "Thanks, paid today."
maildo reply --id 3c1f2a7 --account work --body-file reply.md --markdown
maildo reply --reply-all --id 3c1f2a7 --body "On it" --quote short
maildo reply --query "from:client@example.com subject:quote" --latest --body "Thanks" --send --dry-run
maildo reply --id 3c1f2a7 --body "Preview" --dry-run --json
```

## Flags

- `--account` Account to use (default: the active account)
- `--approve` Approval code from the --dry-run preview; maildo sends only if the message still matches it, and only once
- `--body` Message body as text
- `--body-file` Read the message body from a file, or from stdin with -
- `--draft` Save the reply as a draft (the default)
- `--first` With --query, the same as --latest
- `--full` Show the whole body in the preview, also when it is long
- `--html` Write the body in HTML; maildo removes unsafe parts
- `--id` Message id, or a short id from list or search
- `--internet-id` Message-ID header of the message, such as <id@example.com>
- `--latest` With --query, use the newest match
- `--markdown` Write the body in Markdown; maildo turns it into HTML
- `--query` Search for the message; use with --latest or --first
- `--quote` How much of the original to quote: full, short or none
- `--quote-language` Language of the "On ... wrote:" line: en, de or auto (from your locale)
- `--reply-all` Reply to the sender and all other recipients
- `--send` Send the reply instead of saving a draft
- `--text` Write the body as plain text
- `--yes` Send without asking; needs --approve unless the account allows unattended sends

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Preview changes

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- draft create
- send
- read
