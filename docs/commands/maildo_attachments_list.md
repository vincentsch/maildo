# maildo attachments list

List the attachments of a message or draft

## Usage

```
maildo attachments list [id]
```

## Examples

```
maildo attachments list 3c1f2a7
maildo attachments list --id 3c1f2a7
maildo attachments list --id 3c1f2a7 --account work --json
maildo attachments list --internet-id "<message-id@example.com>" --account work --json
maildo attachments list --query "invoice has:attachment" --latest --all-accounts --plain
maildo attachments list --draft-id 5d2e8b1
```

## Flags

- `--account` Account to use; repeat it for more than one (default: the active account)
- `--all-accounts` Use all your accounts
- `--draft-id` Draft id, or a short id from maildo
- `--first` With --query, the same as --latest
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

- attachments download
- attachments add
- attachments remove
- read
- draft read
