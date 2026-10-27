# maildo restore

Move messages back from Trash or Junk (to the inbox, or to --to)

## Usage

```
maildo restore [id...]
```

## Examples

```
maildo restore 4d1a2f3
maildo restore --id 3c1f2a7 --account work --to Clients
maildo restore 4d1a2f3 9be01c4 --to inbox
maildo restore --id 3c1f2a7 --id 9be01c4 --to inbox
maildo restore --ids-file ids.txt --to inbox --apply --json
maildo restore --query "invoice" --all --to inbox --apply
```

## Flags

- `--account` Account to use (default: the active account)
- `--all` With --query, use every match, up to --max-targets
- `--apply` Make the change; without it, a change to more than one message only shows a preview
- `--first` With --query, the same as --latest
- `--folder` With --query, search only this folder; without it, Sent, Drafts, Trash and Junk are left out (restore looks only in Trash and Junk)
- `--id` Message id, or a short id from list or search (repeatable)
- `--ids-file` Read message ids from a file, one per line, or from stdin with -
- `--internet-id` Message-ID header of the message, such as <id@example.com>
- `--latest` With --query, use the newest match
- `--max-targets` Most messages --all may pick, at most 1000
- `--query` Search for messages; use with --all, --latest or --first
- `--to` Folder to put the messages back in (default: the inbox)

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

- delete
- move
- folders list
