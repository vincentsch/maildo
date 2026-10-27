# maildo draft read

Show a draft

## Usage

```
maildo draft read [id]
```

## Examples

```
maildo draft read 3c1f2a7
maildo draft read --id 3c1f2a7
maildo draft read --id 3c1f2a7 --account work --json
maildo draft read --id 3c1f2a7 --body-mode html
maildo draft read --id 3c1f2a7 --plain
```

## Flags

- `--account` Account to use (default: the active account)
- `--body-mode` How to show the body: readable, text, html or raw
- `--id` Draft id, or the short id maildo printed

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

- draft update
- draft send
- read
