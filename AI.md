

# Org-Mode IMAP Server

A minimalist IMAP server in Go that uses a single org-mode file as its mail store.

## Design

- **`*`** → IMAP namespace root (INBOX equivalent)
- **`**`** → Year folders (e.g., "2026")
- **`***`** → Month/day subfolders (e.g., "2026-06 June/2026-06-12 Friday")
- **`****`** → Individual email messages
- File watching for live org-file changes
- Plain text only; HTML is stripped on incoming mail
- Listens on `127.0.0.1:1143` only

## Usage

### Build and run

```bash
go build -o orgmail main.go
./orgmail mail.org 127.0.0.1:1143
```

### Arguments

| Argument | Default | Description |
|----------|---------|-------------|
| 1st | `mail.org` | Path to org-mode file |
| 2nd | `127.0.0.1:1143` | Listen address (localhost only) |

### Sample `mail.org` file

```org
* 2026
** 2026-06 June
*** 2026-06-11 Thursday
**** TODO [2026-06-11 Thu 14:01] 道同学休假一天
请批准休假申请
**** [2026-06-11 Thu 10:09] 七同学说到: 下周开会
会议内容如下
*** 2026-06-12 Friday
**** [2026-06-12 Fri 15:58] subjects here
email content goes here
```

### Connect with a mail client

Configure any IMAP client:

| Setting | Value |
|---------|-------|
| Server | `127.0.0.1` |
| Port | `1143` |
| Encryption | None |
| Username | anything |
| Password | anything |

Or test with `telnet`/`openssl`:

```bash
telnet 127.0.0.1 1143
a001 LOGIN user pass
a002 LIST "" "*"
a003 SELECT INBOX
a004 FETCH 1:* (FLAGS ENVELOPE)
a005 LOGOUT
```

## Key mapping details

| Org Structure | IMAP Concept |
|---------------|-------------|
| `*` (H1) | Top-level folder (year) |
| `**` (H2) | Subfolder (month) |
| `***` (H3) | Sub-subfolder (day) |
| `****` (H4) | Email message |
| `TODO` keyword | `\Flagged` IMAP flag |
| Timestamp `[...]` | Email Date header |
| Text after timestamp | Email Subject |
| Lines below `****` | Email Body (plain text) |
| `INBOX` (virtual) | All messages across all folders |
| Folder path separator | `/` (e.g. `2026/2026-06 June/2026-06-12 Friday`) |

## Features

- **File watching**: Polls every 3 seconds for external org-file changes; also checks on `NOOP` and `IDLE`
- **IDLE support**: Clients that support IDLE get notified of new messages
- **APPEND**: New emails appended via IMAP are written into the org file under the correct date hierarchy
- **HTML stripping**: Any HTML in incoming messages is automatically stripped to plain text
- **Flag sync**: `\Flagged` ↔ `TODO`, `\Deleted` triggers removal on `EXPUNGE`
- **Bidirectional**: Edit the org file externally (e.g., in Emacs) and changes propagate to connected IMAP clients
