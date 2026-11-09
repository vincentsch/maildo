# maildo attachments add

Add files to a draft

## Usage

```
maildo attachments add <path...>
```

## Examples

```
maildo attachments add --draft-id 5d2e8b1 ./quote.pdf
maildo attachments add --draft-id 5d2e8b1 --account work ./quote.pdf --json
maildo attachments add --draft-id 5d2e8b1 ./quote.pdf --dry-run
```

## Flags

- `--account` Account to use (default: the active account)
- `--draft-id` Draft id, or a short id from maildo

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

- attachments list
- attachments remove
- draft read
