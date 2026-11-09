# maildo auth refresh

Renew an account's saved login without asking you

## Usage

```
maildo auth refresh <name>
```

## Examples

```
maildo auth refresh work
maildo auth refresh work --dry-run
maildo auth refresh work --json
```

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Preview changes

This command changes things. Add `--dry-run` to see what it would change; nothing changes then.

## Request details

Add `--include-meta` to the JSON output to get the result as `{data, meta}`, where `meta` holds details about the request.

## Related commands

- auth status
- auth repair
