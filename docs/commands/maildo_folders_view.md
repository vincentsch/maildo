# maildo folders view

Show one folder

## Usage

```
maildo folders view <folder>
```

## Examples

```
maildo folders view inbox
maildo folders view "Clients/ACME" --account work
maildo folders view inbox --json
```

## Flags

- `--account` Account to use (default: the active account)

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

- folders list
