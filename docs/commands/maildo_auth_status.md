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

This command makes changes. Use `--dry-run` to see what would change without changing anything.

## Metadata

This command can attach request metadata. In advanced-output apps, add `--include-meta` to a supported machine output mode to wrap the result as `{data, meta}`.

## Related commands

- auth refresh
- auth repair
- auth list
