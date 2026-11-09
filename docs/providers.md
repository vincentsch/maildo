# Providers

maildo can connect to a mail service in three ways. Pick the guide for your
account.

| Your account | Guide | How maildo connects | Status |
| --- | --- | --- | --- |
| Gmail (the easiest way), iCloud, Yahoo, Fastmail, GMX, Posteo, mailbox.org, a company or self-hosted server | [IMAP and SMTP](providers/imap.md) | IMAP for reading, SMTP for sending, with a password or app password | tested against Dovecot and GreenMail servers |
| Outlook.com, Hotmail, Microsoft 365 | [Outlook](providers/outlook.md) | Microsoft Graph, with an app registration you create once | tested live with a Microsoft 365 work account; Outlook.com not yet |
| Gmail through Google's API | [Gmail](providers/gmail.md) | Gmail API, with a Google Cloud OAuth app you create once | tested live with a Google Workspace account; a personal Gmail account not yet |

`maildo login` starts with your address and picks the right way for you.
For an Outlook address, it first explains the app registration.

## What each way can do

| | IMAP and SMTP | Outlook | Gmail API |
| --- | --- | --- | --- |
| List, search, read, threads | yes | yes | yes |
| Attachments | yes | yes | yes |
| Folders (labels on Gmail) | yes | yes | yes |
| Mark, flag, move, archive, delete, junk, restore | yes | yes | yes |
| Drafts, reply, forward, send | yes | yes | yes |
| Categories | no | yes | as user labels |
| Inbox rules | no | yes | no |
| Saved login | password in a local file | refresh token in a local file | refresh token in a local file |

The search runs on the provider's side. IMAP servers match words anywhere
in the message. Outlook and Gmail use their own search, which works like
the search box in their apps. So results can differ a little between
providers.

## Not supported yet

- POP3 and Exchange EWS
- JMAP (Fastmail works over IMAP)
- Passwords from a password manager (`pass`, 1Password) or from the system
  keychain. Saved logins are files that only your user can read; see
  [SECURITY.md](../SECURITY.md).
