# maildo categories list

List the categories of a message (on Gmail, your own labels)

## Usage

```
maildo categories list [id]
```

## Examples

```
maildo categories list --id 3c1f2a7
maildo categories list --internet-id "<message-id@example.com>" --account work --json
maildo categories list --query "invoice" --latest --json
```

## Flags

- `--account` Account to use (default: the active account)
- `--first` With --query, use the first match; matches come newest first, so this is the same as --latest
- `--folder` With --query, search only this folder; without it, Sent, Drafts, Trash and Junk are left out (restore looks only in Trash and Junk)
- `--id` Message id, or a short id from list or search
- `--internet-id` Message-ID header of the message, such as <id@example.com>
- `--latest` With --query, use the newest match
- `--query` Search for the message; use with --latest or --first

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- categories set
- categories clear
