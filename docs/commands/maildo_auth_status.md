# maildo auth status

Show whether an account has a working login

## Usage

```
maildo auth status [name]
```

## Examples

```
maildo auth status
maildo auth status work
maildo auth status work --refresh --dry-run
maildo auth status work --json
```

## Flags

- `--refresh` Also renew the login, to check that it still works

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

- auth refresh
- auth repair
- auth list
