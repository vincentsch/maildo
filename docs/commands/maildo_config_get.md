# maildo config get

Show one setting

## Usage

```
maildo config get <setting>
```

## Examples

```
maildo config get outlook.client_id
maildo config get policy.read_only --account work
```

## Flags

- `--account` Account the setting is for (default: the active account)

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Related commands

- config set
- config list
