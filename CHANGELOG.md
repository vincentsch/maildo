# Changelog

## Unreleased

This is the first public release of maildo. It runs on Linux and macOS.
Windows is not supported yet, but you can use maildo inside WSL.

### Accounts

- `maildo login` asks for your address and finds the servers. It knows the
  large services, and for other domains it checks the domain's autoconfig
  file and Thunderbird's provider database. After that it looks at the DNS
  SRV and MX records.
- Supported accounts: IMAP and SMTP with a password or app password,
  Fastmail, and Outlook and Gmail through their APIs with your own OAuth
  app.
- `--ca-file` for servers with their own certificate authority.
- When maildo cannot find the servers for a domain, `maildo login` asks for
  them and suggests the usual ports and TLS modes.
- Outlook and Gmail sign-in print the sign-in address and wait for you.
  Gmail login checks for the client secret before it opens the browser.
- `maildo doctor` checks the connection one step at a time and lists what
  works. A failed check is explained like any other error.
- `maildo config` sets the Outlook and Gmail client ids, the sender name
  and the account policies.
- `maildo auth` manages your accounts, and `auth repair` logs in again.
- `maildo update` checks for a new version and installs it.

### Reading

- `list` and `search` with pages (`--limit` up to 1000, and `--page`). In
  JSON, each message has a short preview. When one account or one message
  fails, you get the other results and a warning.
- Search runs on the server. It understands words and phrases, the
  operators `from:`, `to:`, `cc:`, `subject:`, `has:attachment`,
  `is:unread` and `is:flagged`, and dates and ages such as `newer:7d`.
  Trash and Junk are left out on every provider unless you ask for them.
  A repeated operator matches any of its values, and `OR` between two
  values of the same operator means the same, as in
  `from:anna OR from:ben`. Other uses of `OR`, and `AND`, `NOT` and
  `-word`, are refused with a hint, so they are never searched as words by
  mistake.
- Gmail search fetches only the page it shows. Outlook word searches that
  reach Outlook's limit of 1,000 results show a warning.
- `read`, `thread` and attachment download. For broken mail, maildo shows
  what it can decode, with a warning. Attachment names written in the
  RFC 2231 and RFC 2047 formats, for example "Übersicht Q3.pdf", come
  through correctly.
- Tables show sender names, folders and local dates, and short ids that
  later commands accept. A short id gets longer than 7 characters when two
  messages start the same way. A short id that matches two messages is
  refused, and the error shows both.
- A short id is never given to a second message. When maildo forgets an
  old message, its short id is refused from then on.
- Every command that takes `--id` also takes the message id or short id as
  an argument, like `maildo read 9eac029` or
  `maildo attachments list 9eac029`.

### Changing and sending

- Mark, flag, move, copy, archive, delete, junk and restore messages, one
  at a time or every match of a search. The preview shows the folder of
  each message. It prints a command that changes exactly those messages in
  that account, never a new search. For many messages, that command reads
  their ids from a file, and refuses the file if it changed after the
  preview.
- When a change moves mail to another folder, as `archive` and `delete`
  do, maildo prints the command that moves the messages back to the
  folders they came from. The JSON has
  it in `undo_commands`, and the ids after the change in `new_id`.
- Create, rename and delete folders (on Gmail, labels). In this beta,
  `folders delete` deletes only empty folders without subfolders.
- `maildo attachments add` and `attachments remove` change the files of a
  draft.
- `maildo draft delete` discards a draft like the discard button of a mail
  program: the draft is gone for good and does not go to Trash. It takes
  only drafts and needs `--confirm` with the draft's id, which the
  `--dry-run` preview prints.
- `maildo rules` manages Outlook Inbox rules. `rules health` finds rules
  with problems, and creating or changing a rule needs `--apply`.
- `maildo categories` shows and sets Outlook categories, and your own
  labels on the Gmail API.
- `maildo outlook archive-policy check` shows the archive and retention
  settings maildo can see.
- When a command that changes mail uses `--query`, it leaves out Sent,
  Drafts, Trash and Junk unless `--folder` names a folder.
  `restore --query` looks in Trash and Junk.
- Drafts, replies, reply to all, forwards, and `send` with `--attach`.
  Forwards take the original's attachments along unless you pass
  `--include-attachments none`. For IMAP accounts, maildo saves a copy in
  Sent, and only that copy has the Bcc header.
- With `--dry-run`, a send shows the whole message, with every attachment
  and inline image. Empty fields such as Cc show "(none)", and are empty
  lists in JSON. A reply shows the Message-ID it answers. When From has no
  name, the preview says how to add one. `--full` shows long bodies in full, and JSON always has
  the whole body. The preview prints an approval code and the command that
  sends exactly that message. In a terminal maildo asks before it sends.
  Without a terminal, a send needs `--approve` with the code and `--yes`.
  A message that changed after its preview is refused, also when it
  changes while maildo sends it.
- An approval code sends its message once. Running the printed command a
  second time is refused with `approval_used`, also when maildo could not
  tell whether the first send went out. A new preview of the same message
  says it went out before and lets the code send it once more.
- Printed send commands name the account, so switching accounts in between
  does not change who sends.
- Every command maildo prints for the next step can be pasted into a shell
  as it is, whatever its arguments hold. A line break in a body stays a
  line break inside the quotes, and an argument that starts with a dash
  goes after `--`. A command with a tab or another character a terminal
  cannot show is not printed in the terminal; the JSON has it.
- Gmail sends a draft with the raw message maildo checked. Outlook sends
  every message with sendMail, a draft as the copy maildo checked, and
  `send --attach` works on Outlook up to 3 MB of attachments. On Outlook,
  `reply --send --dry-run` saves the reply as a draft and shows that draft.
- A quoted reply starts the quote with "On <date>, <sender> wrote:", in
  English or German (`--quote-language`). One empty line separates your
  text from the quote, also when the body file ends with a line break.
- Replies, forwards and draft sends use your `sender_name`. `--subject`
  refuses line breaks and control characters.
- Account policies: `read_only`, `drafts_only`, `allow_recipients`,
  `max_bulk` and `unattended_send`. Only a person at a terminal can loosen
  a policy. The docs explain that policies stop mistakes but are not a
  sandbox.
- A local action log records every change to messages, drafts, folders
  and rules, and every send. It also has the replies and forwards that
  maildo saved as drafts. Results tell you when maildo could not write the
  log.

### For scripts and agents

- Stable `--json` output for every command, `--jq` that prints strings
  without quotes, and `--template`.
- Errors give a cause, a next step and a stable `code`. For changes, an
  `outcome` says whether it is safe to try again. This works on every
  provider and for every kind of change, so a lost answer from the server
  does not lead to a second send. A success answer from Outlook that maildo
  cannot read also counts as unknown.
- `--ids-file -` reads ids from stdin.
- `AGENTS.md` and a Claude Code skill in `skills/maildo`. Both tell an
  agent that mail text is data and never instructions, and that it shows
  every send to the user first.
