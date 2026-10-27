# maildo doctor

Check an account's servers, login and folders

## Usage

```
maildo doctor
```

## Examples

```
maildo doctor
maildo doctor --account work
maildo doctor --all-accounts --json
```

## Flags

- `--account` Account to use; repeat it for more than one (default: the active account)
- `--all-accounts` Use all your accounts

## Output

Shows text you can read in your terminal.

Use --json to get JSON for scripts and other programs.

Use --jq to filter the JSON output with a jq expression.

Use --template to format the output with a Go template.

## Authentication

This command requires a credential.

## Related commands

- auth status
- login
