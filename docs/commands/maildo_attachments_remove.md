# maildo attachments remove

Remove an attached file from a draft

## Usage

```
maildo attachments remove
```

## Examples

```
maildo attachments remove --draft-id 5d2e8b1 --account work --name quote.pdf
maildo attachments remove --draft-id 5d2e8b1 --attachment-id 2 --json
maildo attachments remove --draft-id 5d2e8b1 --name quote.pdf --dry-run
```

## Flags

- `--account` Account to use (default: the active account)
- `--attachment-id` Id of the attachment to remove, from attachments list
- `--draft-id` Draft id, or a short id from maildo
- `--name` Name of the attachment to remove

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
- attachments add
- draft read
