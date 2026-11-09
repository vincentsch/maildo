# maildo read

Read a message

## Usage

```
maildo read [id]
```

## Examples

```
maildo read 4d1a2f3
maildo read --id 3c1f2a7
maildo read --internet-id "<message-id@example.com>" --account work
maildo read --query "from:client@example.com subject:quote" --latest
maildo read --query "invoice has:attachment" --latest --all-accounts --json
```

## Flags

- `--account` Account to use; repeat it for more than one (default: the active account)
- `--all-accounts` Use all your accounts
- `--body-mode` How to show the body: readable, text, html or raw
- `--first` With --query, use the first match; matches come newest first, so this is the same as --latest
- `--id` Message id, or a short id from list or search
- `--internet-id` Message-ID header of the message, such as <id@example.com>
- `--latest` With --query, use the newest match
- `--query` Search for the message; use with --latest or --first

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --plain to get text without color or formatting.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- thread
- search
- list
