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

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- attachments list
- attachments remove
- draft read
