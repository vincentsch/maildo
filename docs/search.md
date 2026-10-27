# Search syntax

`maildo search` and every `--query` option use the same small search
language.

```sh
maildo search "from:anna invoice newer:30d"
maildo search 'subject:"quarterly report" has:attachment'
maildo archive --query "from:news@shop.example older:7d" --all
```

## Words and phrases

A message matches only when all plain words appear in it. To match a phrase
as you wrote it, put it in quotes:

```text
invoice march              both words, anywhere in the message
"project kickoff"          the exact phrase
```

## Operators

| Operator | Matches |
| --- | --- |
| `from:anna` | the sender's address or name contains "anna" |
| `to:team@example.com` | a To recipient |
| `cc:boss` | a Cc recipient |
| `subject:invoice` | the subject contains the word |
| `subject:"march invoice"` | the subject contains the phrase |
| `has:attachment` | messages with at least one attachment |
| `is:unread` / `is:read` | read state |
| `is:flagged` / `is:unflagged` | flagged (starred in Gmail) |
| `after:2026-01-31` | arrived on or after that day |
| `before:2026-02-01` | arrived before that day |
| `newer:7d` | arrived in the last 7 days |
| `older:2w` | arrived more than 2 weeks ago |

`after:` and `before:` take a date as `YYYY-MM-DD`, `today`, `yesterday` or
an age. `newer:` and `older:` take only an age. An age is a number with
`d` for days, `w` for weeks, `m` for months or `y` for years, such as `7d`
or `3m`. `newer_than:` and `older_than:` work the same as `newer:` and
`older:`, as in Gmail. Dates count in UTC.

## Combining

When you use different operators and words, all of them must match:

```text
from:anna has:attachment newer:30d
```

When you repeat an operator, it matches any of its values:

```text
from:anna@example.com from:ben@example.com     mail from either of them
```

There is no `OR` keyword. If you write `OR`, maildo stops with an error
instead of searching for the word "OR". To search for the word, quote it
(`"OR"`).

There is no way to exclude something yet. `NOT invoice` and `-invoice`
would turn into a search for those words, so maildo refuses them. It also
refuses `AND`, because every word has to match anyway. Quote them (`"NOT"`,
`"-invoice"`) to search for the text. Lowercase "and" and "not" are normal
words, and so is a word with a hyphen inside, like `e-mail`.

An unknown operator such as `label:work` is an error. maildo never treats
it as a plain word. Quote it to search for the text.

## Where maildo searches

- `search` looks in every folder except Trash and Junk, on every provider.
  On Outlook those are Deleted Items and Junk Email, and on Gmail Trash and
  Spam. Add `--folder Trash` to search there.
- Without `--folder`, the FOLDER column shows where each message is.
- On Gmail over IMAP, maildo searches only "All Mail", so a message with
  several labels shows up once. Gmail does not put Trash and Spam in All
  Mail.
- `--account name` picks an account, and `--all-accounts` searches all of
  them. If one account fails while others answer, maildo leaves it out and
  prints a warning.

## Results

Results come newest first, 25 per page. `--limit` takes up to 1000, and
`--page 2` shows the next page. With `--json`, `total` says how many
messages matched, and `has_more` says whether there is another page.

```sh
maildo search "from:billing" --json --jq '.messages[] | .id'
```

IMAP servers compare dates in their own time zone. So maildo asks the
server for one more day on each side and then checks the exact arrival
time itself. When a folder has more than 20,000 matches, maildo returns the
newest 20,000 with a warning. To see the rest, narrow the search with a
date.

Outlook returns at most 1,000 results for a word search. When a search
reaches that limit, maildo shows what came back, with a warning that older
matches may be missing. To see the rest, add a date such as `newer:30d` or
a sender, or name a folder with `--folder`. Searches that use only `is:`,
`has:` and dates have no such limit.

Some commands pick the messages to change with a search, as
`archive --query ... --all` does. For those, a search that had to leave
matches out is an error instead of a warning. That way maildo never changes
a list that it knows is incomplete.

## Providers

The operators mean the same on every provider, but the provider does the
matching. Most IMAP servers match text anywhere in the field, so `from:ann`
also finds joanna@example.com. Some look only at the address and not at
the name. Outlook and Gmail use their own search engines
with their own rules, much like the search box in their apps. So the same
search can give slightly different results on different providers.
