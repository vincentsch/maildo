# IMAP and SMTP accounts

Most mail services and every self-hosted mail server use IMAP for reading
mail and SMTP for sending it. maildo uses both. This page covers the
services in the table below, including Gmail with an app password, and your
own server.

> **Status:** tested against Dovecot and GreenMail servers in Docker. The
> settings for the services below use the server names from each service's
> help pages.

## Log in

```sh
maildo login
```

Type your address. For the services below, maildo already knows the
servers. For any other address, it tries these in order:

1. the domain's autoconfig file (`autoconfig.<domain>`)
2. Thunderbird's provider database
3. DNS SRV records for IMAP and submission (RFC 6186)
4. the domain's MX records, which often point to a hosting company maildo
   knows
5. the usual host names, such as `imap.<domain>`, `smtp.<domain>` or
   `mail.<domain>`, if a connection to them works

maildo prints where it found the servers. If nothing works, it asks you for
them.

In a script, give the password in `MAILDO_IMAP_PASSWORD`:

```sh
MAILDO_IMAP_PASSWORD='app-password' maildo login --email me@example.com --name work
```

maildo uses this one password for IMAP and for SMTP, and logs in to both
with the same username. There is no separate SMTP password. A server that
wants a different password for sending does not work with maildo yet.

To give the servers yourself, use `--imap-host` and `--smtp-host`. The ports
are 993 for IMAP and 465 for SMTP unless you set them. On 993 and 465,
maildo uses TLS from the start, and on 143 and 587 it uses STARTTLS.
`--imap-tls` and `--smtp-tls` (`implicit` or `starttls`) override that.
`--username` sets a login name that is not your address.

maildo never sends a password without encryption, and it always checks the
server's certificate.

## Services maildo knows

| Service | Addresses | Password |
| --- | --- | --- |
| Gmail | gmail.com, googlemail.com | app password ([create one](https://myaccount.google.com/apppasswords); needs 2-Step Verification) |
| iCloud Mail | icloud.com, me.com, mac.com | app-specific password, at [account.apple.com](https://account.apple.com) under Sign-In and Security |
| Yahoo Mail | yahoo.*, ymail.com, rocketmail.com | app password, at [login.yahoo.com/account/security](https://login.yahoo.com/account/security) |
| AOL Mail | aol.com, aim.com | app password, at [login.aol.com/account/security](https://login.aol.com/account/security) |
| Fastmail | fastmail.com and its other domains | app password with IMAP and SMTP access (Settings > Privacy & Security > App passwords) |
| GMX | gmx.de, gmx.net, gmx.com and others | your GMX password; turn on IMAP first under Settings > POP3 & IMAP |
| WEB.DE | web.de | your WEB.DE password; turn on IMAP first under Settings > POP3/IMAP |
| Posteo | posteo.de and its other domains | your Posteo password |
| mailbox.org | mailbox.org | your password, or an app password if you use two-factor login |

Outlook.com, Hotmail and Microsoft 365 addresses no longer accept passwords
over IMAP. They need the [Outlook setup](outlook.md). Gmail also works
through the Gmail API; [Gmail](gmail.md) explains both ways.

Fastmail accounts use the provider name `fastmail`, which always uses
Fastmail's servers. Fastmail's Basic plan has no IMAP access.

## Your own server

Tell maildo where it is:

```sh
maildo login --email me@example.org --imap-host mail.example.org --smtp-host mail.example.org
```

maildo uses port 993 for IMAP and 465 for SMTP, both with TLS from the
start, unless you pass `--imap-port`, `--smtp-port` or a TLS mode. In a
terminal you can also run plain `maildo login`. When maildo cannot find the
servers for your domain, it says so and asks for them. For the port and the
TLS mode it suggests the usual values, and you can press Enter to take
them:

```text
maildo could not find the mail servers for me@example.org; enter them below or pass --imap-host and --smtp-host.
IMAP host: mail.example.org
IMAP port [993]:
IMAP TLS mode, implicit or starttls [implicit]:
SMTP host: mail.example.org
SMTP port [465]:
SMTP TLS mode, implicit or starttls [implicit]:
```

If the server's certificate comes from your own certificate authority, add
`--ca-file /path/to/ca.pem`. maildo then trusts that authority for this
account, in addition to the ones your system trusts.

A server on other ports, with a company certificate authority, looks like
this. IMAP runs on port 1993 and SMTP on port 1465, both with TLS from the
start:

```sh
MAILDO_IMAP_PASSWORD='your-password' maildo login --email me@example.org \
  --imap-host mail.example.org --imap-port 1993 \
  --smtp-host mail.example.org --smtp-port 1465 \
  --ca-file ./company-ca.pem
```

maildo picks STARTTLS by itself only on the ports that usually have it,
such as 143 and 587. On any other port it uses TLS from the start. When your server wants STARTTLS there, add
`--imap-tls starttls` or `--smtp-tls starttls`. For example, IMAP with
STARTTLS on port 1143:

```sh
maildo login --email me@example.org \
  --imap-host mail.example.org --imap-port 1143 --imap-tls starttls \
  --smtp-host mail.example.org --smtp-port 1465 \
  --ca-file ./company-ca.pem
```

maildo saves the full path to the CA file in its config, so leave the file
where it is.

`maildo doctor` shows what your server supports. Here is its output for the
Dovecot server that maildo's tests use:

```text
$ maildo doctor
work (imap, me@example.test)
  ok       DNS      localhost is 127.0.0.1
  ok       TLS      TLS 1.3 on localhost:39993, certificate valid until 25 Nov 2026 (issued by maildo test CA)
  ok       Login    the IMAP server accepted the password
  ok       IMAP     IMAP4rev1, SPECIAL-USE, MOVE, UIDPLUS, CONDSTORE, ESEARCH, LIST-STATUS, IDLE, SORT
  ok       Folders  9 folders; Sent=Sent, Drafts=Drafts, Archive=Archive, Trash=Trash, Junk=Junk, marked by the server (SPECIAL-USE)
  ok       SMTP     localhost:39465 accepted the login
  Works: search, list, read, thread, attachments, mark, flag, folders, copy, move, restore, archive, delete, junk, send, reply, forward, drafts
```

## What works

All of maildo's mail commands work over IMAP, including sending and drafts.
Categories and Inbox rules are not available over IMAP.

A few things depend on the server:

- Standard folders: archive, delete and junk move mail to the server's
  Archive, Trash and Junk folders. maildo finds them by the server's
  SPECIAL-USE marks, or by their usual names. If one is missing, create it,
  for example with `maildo folders create Archive`.
- Moving needs the IMAP extensions MOVE and UIDPLUS. Nearly every current
  server has them, and `maildo doctor` tells you if yours does not.
- After sending, maildo saves a copy in the Sent folder. Gmail and Microsoft
  save that copy themselves, so maildo leaves it to them.
- IMAP has no special support for drafts. A draft is a message in the
  Drafts folder. When you change a draft, maildo saves a new message and
  deletes the old one, so the draft gets a new id. maildo prints it. On a
  server without UIDPLUS, maildo can only mark the old draft as deleted and
  tells you so. `maildo draft delete` removes a draft the same way, and it
  does not go to Trash.

## How maildo uses IMAP

- Reading never changes a message. maildo fetches messages with
  `BODY.PEEK`, so they stay unread.
- The server does the searching. maildo asks it once per folder, sorts the
  matches by date and fetches details only for the page you see. So a large
  folder costs a few round trips, and not one per message.
- Messages that break the MIME rules, for example with broken encodings or
  unknown character sets, still show up. maildo decodes what it can and
  adds a warning.
- A message id contains the folder, the folder's UIDVALIDITY and the
  message's UID. If the server renumbers a folder, old ids stop working.
  maildo then says so and does not pick another message.
- maildo uses one connection for the whole command and logs out at the end.
