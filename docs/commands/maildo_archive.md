# maildo archive

Archive messages

## Usage

```
maildo archive [id...]
```

## Examples

```
maildo archive --id 3c1f2a7 --account work
maildo archive 4d1a2f3 9be01c4
maildo archive --id 3c1f2a7 --id 9be01c4
maildo archive --ids-file ids.txt --apply --json
maildo archive --query "invoice" --all --max-targets 50 --apply
```

## Flags

- `--account` Account to use (default: the active account)
- `--all` With --query, use every match, up to --max-targets
- `--apply` Make the change; without it, a change to more than one message only shows a preview
- `--first` With --query, use the first match; matches come newest first, so this is the same as --latest
- `--folder` With --query, search only this folder; without it, Sent, Drafts, Trash and Junk are left out (restore looks only in Trash and Junk)
- `--id` Message id, or a short id from list or search (repeatable)
- `--ids-file` Read message ids from a file, one per line, or from stdin with -
- `--internet-id` Message-ID header of the message, such as <id@example.com>
- `--latest` With --query, use the newest match
- `--max-targets` Most messages --all may pick, at most 1000
- `--query` Search for messages; use with --all, --latest or --first

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

- move
- delete
- junk
