# maildo attachments download

Save a message's attachments to a folder

## Usage

```
maildo attachments download [id]
```

## Examples

```
maildo attachments download 3c1f2a7 --all --to ./attachments
maildo attachments download --id 3c1f2a7 --account work --all --to ./attachments
maildo attachments download --id 3c1f2a7 --attachment-id 1 --to ./attachments --json
maildo attachments download --id 3c1f2a7 --name invoice.pdf --to ./attachments
maildo attachments download --query "invoice has:attachment" --latest --attachment-id 2 --to ./attachments --json
maildo attachments download --id 3c1f2a7 --all --to ./attachments --dry-run
```

## Flags

- `--account` Account to use (default: the active account)
- `--all` Save every attached file
- `--attachment-id` Id of an attachment to save, from attachments list (repeatable)
- `--first` With --query, use the first match; matches come newest first, so this is the same as --latest
- `--id` Message id, or a short id from list or search
- `--internet-id` Message-ID header of the message, such as <id@example.com>
- `--latest` With --query, use the newest match
- `--name` Name of an attachment to save (repeatable)
- `--overwrite` Replace files that already exist in the folder
- `--query` Search for the message; use with --latest or --first
- `--to` Folder to save the attachments in (created if missing)

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

- attachments list
- read
- search
