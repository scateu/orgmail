// main.go
package main

import (
	"bufio"
	"crypto/md5"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Org-mode data model
// ---------------------------------------------------------------------------

// OrgMessage represents a single **** heading = one email message.
type OrgMessage struct {
	UID       uint32
	Date      time.Time
	Subject   string
	Body      string
	Flags     []string // e.g. "\\Seen", "\\Flagged"
	IsTODO    bool
	RawHeader string // the **** line as-is (minus leading stars)
}

// OrgDay represents a *** heading (day grouping).
type OrgDay struct {
	Title    string // e.g. "2026-06-12 Friday"
	Messages []*OrgMessage
}

// OrgMonth represents a ** heading (month grouping).
type OrgMonth struct {
	Title string // e.g. "2026-06 June"
	Days  []*OrgDay
}

// OrgYear represents a * heading (year grouping).
type OrgYear struct {
	Title  string // e.g. "2026"
	Months []*OrgMonth
}

// OrgStore is the entire mail store backed by one org file.
type OrgStore struct {
	mu       sync.RWMutex
	filePath string
	years    []*OrgYear
	nextUID  uint32
	modTime  time.Time

	// UID validity counter – bumped when we do a full reload that
	// might re-assign UIDs (we try not to).
	uidValidity uint32

	// Global UID -> message pointer for fast lookup
	uidMap map[uint32]*OrgMessage

	// For file-change detection
	lastHash [16]byte
}

func NewOrgStore(path string) *OrgStore {
	s := &OrgStore{
		filePath:    path,
		nextUID:     1,
		uidValidity: uint32(time.Now().Unix()),
		uidMap:      make(map[uint32]*OrgMessage),
	}
	return s
}

// Load reads and parses the org file.
func (s *OrgStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *OrgStore) loadLocked() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			s.years = nil
			return nil
		}
		return err
	}

	newHash := md5.Sum(data)
	if newHash == s.lastHash {
		return nil // no change
	}
	s.lastHash = newHash

	info, _ := os.Stat(s.filePath)
	if info != nil {
		s.modTime = info.ModTime()
	}

	// Re-parse
	s.years = nil
	s.uidMap = make(map[uint32]*OrgMessage)
	s.nextUID = 1

	lines := strings.Split(string(data), "\n")
	s.parseLines(lines)
	return nil
}

var (
	reH1 = regexp.MustCompile(`^\*\s+(.+)$`)
	reH2 = regexp.MustCompile(`^\*\*\s+(.+)$`)
	reH3 = regexp.MustCompile(`^\*\*\*\s+(.+)$`)
	reH4 = regexp.MustCompile(`^\*\*\*\*\s+(.+)$`)

	// Match the timestamp + subject in a **** line
	// e.g. "TODO [2026-06-11 Thu 14:01] 道同学休假一天"
	// or   "[2026-06-12 Fri 15:58] subjects here"
	reH4Detail = regexp.MustCompile(`^(TODO\s+)?\[(\d{4}-\d{2}-\d{2}\s+\w+\s+\d{1,2}:\d{2})\]\s*(.*)$`)
)

func (s *OrgStore) parseLines(lines []string) {
	var curYear *OrgYear
	var curMonth *OrgMonth
	var curDay *OrgDay
	var curMsg *OrgMessage
	var bodyLines []string

	flushMsg := func() {
		if curMsg != nil && curDay != nil {
			curMsg.Body = strings.TrimRight(strings.Join(bodyLines, "\n"), "\n ")
			curDay.Messages = append(curDay.Messages, curMsg)
			s.uidMap[curMsg.UID] = curMsg
		}
		curMsg = nil
		bodyLines = nil
	}

	for _, line := range lines {
		// Check headings from most-specific to least
		if m := reH4.FindStringSubmatch(line); m != nil {
			flushMsg()
			rest := strings.TrimSpace(m[1])
			msg := &OrgMessage{
				UID:       s.nextUID,
				RawHeader: rest,
			}
			s.nextUID++

			if dm := reH4Detail.FindStringSubmatch(rest); dm != nil {
				msg.IsTODO = strings.TrimSpace(dm[1]) == "TODO"
				if msg.IsTODO {
					msg.Flags = append(msg.Flags, "\\Flagged")
				}
				t, err := time.Parse("2006-01-02 Mon 15:04", dm[2])
				if err != nil {
					t = time.Now()
				}
				msg.Date = t
				msg.Subject = dm[3]
			} else {
				msg.Subject = rest
				msg.Date = time.Now()
			}

			curMsg = msg
			bodyLines = nil
			continue
		}

		if m := reH3.FindStringSubmatch(line); m != nil {
			flushMsg()
			curDay = &OrgDay{Title: strings.TrimSpace(m[1])}
			if curMonth != nil {
				curMonth.Days = append(curMonth.Days, curDay)
			}
			continue
		}

		if m := reH2.FindStringSubmatch(line); m != nil {
			flushMsg()
			curDay = nil
			curMonth = &OrgMonth{Title: strings.TrimSpace(m[1])}
			if curYear != nil {
				curYear.Months = append(curYear.Months, curMonth)
			}
			continue
		}

		if m := reH1.FindStringSubmatch(line); m != nil {
			flushMsg()
			curDay = nil
			curMonth = nil
			curYear = &OrgYear{Title: strings.TrimSpace(m[1])}
			s.years = append(s.years, curYear)
			continue
		}

		// Body line for current message
		if curMsg != nil {
			bodyLines = append(bodyLines, line)
		}
	}
	flushMsg()
}

// Save writes the org store back to the file.
func (s *OrgStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *OrgStore) saveLocked() error {
	var sb strings.Builder
	for i, y := range s.years {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("* ")
		sb.WriteString(y.Title)
		sb.WriteString("\n")
		for _, mo := range y.Months {
			sb.WriteString("** ")
			sb.WriteString(mo.Title)
			sb.WriteString("\n")
			for _, d := range mo.Days {
				sb.WriteString("*** ")
				sb.WriteString(d.Title)
				sb.WriteString("\n")
				for _, msg := range d.Messages {
					sb.WriteString("**** ")
					if msg.IsTODO {
						sb.WriteString("TODO ")
					}
					sb.WriteString(fmt.Sprintf("[%s] %s\n",
						msg.Date.Format("2006-01-02 Mon 15:04"),
						msg.Subject))
					if msg.Body != "" {
						sb.WriteString(msg.Body)
						sb.WriteString("\n")
					}
				}
			}
		}
	}

	data := []byte(sb.String())
	s.lastHash = md5.Sum(data)

	return os.WriteFile(s.filePath, data, 0644)
}

// ---------------------------------------------------------------------------
// Folder abstraction
// ---------------------------------------------------------------------------

// FolderPath returns a flat list of IMAP-style folder names.
// e.g. "INBOX", "2026", "2026/2026-06 June", "2026/2026-06 June/2026-06-12 Friday"
func (s *OrgStore) FolderPaths() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var paths []string
	paths = append(paths, "INBOX") // virtual top-level
	for _, y := range s.years {
		yp := y.Title
		paths = append(paths, yp)
		for _, mo := range y.Months {
			mp := yp + "/" + mo.Title
			paths = append(paths, mp)
			for _, d := range mo.Days {
				dp := mp + "/" + d.Title
				paths = append(paths, dp)
			}
		}
	}
	return paths
}

// MessagesInFolder returns all messages that live in a particular folder path.
// Only *** (day) level folders contain messages.
// INBOX returns ALL messages across all folders.
func (s *OrgStore) MessagesInFolder(folder string) []*OrgMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if strings.EqualFold(folder, "INBOX") {
		return s.allMessages()
	}

	parts := strings.Split(folder, "/")
	switch len(parts) {
	case 1:
		// Year folder – return all msgs in that year
		for _, y := range s.years {
			if y.Title == parts[0] {
				return s.yearMessages(y)
			}
		}
	case 2:
		for _, y := range s.years {
			if y.Title == parts[0] {
				for _, mo := range y.Months {
					if mo.Title == parts[1] {
						return s.monthMessages(mo)
					}
				}
			}
		}
	case 3:
		for _, y := range s.years {
			if y.Title == parts[0] {
				for _, mo := range y.Months {
					if mo.Title == parts[1] {
						for _, d := range mo.Days {
							if d.Title == parts[2] {
								return d.Messages
							}
						}
					}
				}
			}
		}
	}
	return nil
}

func (s *OrgStore) allMessages() []*OrgMessage {
	var msgs []*OrgMessage
	for _, y := range s.years {
		msgs = append(msgs, s.yearMessages(y)...)
	}
	return msgs
}

func (s *OrgStore) yearMessages(y *OrgYear) []*OrgMessage {
	var msgs []*OrgMessage
	for _, mo := range y.Months {
		msgs = append(msgs, s.monthMessages(mo)...)
	}
	return msgs
}

func (s *OrgStore) monthMessages(mo *OrgMonth) []*OrgMessage {
	var msgs []*OrgMessage
	for _, d := range mo.Days {
		msgs = append(msgs, d.Messages...)
	}
	return msgs
}

// AppendMessage adds a new message to the appropriate day folder (creating
// year/month/day headings as needed). HTML is stripped.
func (s *OrgStore) AppendMessage(folder string, date time.Time, subject, body string, flags []string) (*OrgMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	body = stripHTML(body)
	if !utf8.ValidString(body) {
		body = strings.ToValidUTF8(body, "?")
	}
	if !utf8.ValidString(subject) {
		subject = strings.ToValidUTF8(subject, "?")
	}

	isTODO := false
	for _, f := range flags {
		if f == "\\Flagged" {
			isTODO = true
		}
	}

	msg := &OrgMessage{
		UID:     s.nextUID,
		Date:    date,
		Subject: subject,
		Body:    body,
		Flags:   flags,
		IsTODO:  isTODO,
	}
	s.nextUID++
	s.uidMap[msg.UID] = msg

	// Determine target day
	yearTitle := fmt.Sprintf("%d", date.Year())
	monthTitle := date.Format("2006-01") + " " + date.Format("January")
	dayTitle := date.Format("2006-01-02") + " " + date.Format("Monday")

	// If a specific folder was given and it's a 3-level path, use it
	if folder != "" && !strings.EqualFold(folder, "INBOX") {
		parts := strings.Split(folder, "/")
		if len(parts) >= 1 {
			yearTitle = parts[0]
		}
		if len(parts) >= 2 {
			monthTitle = parts[1]
		}
		if len(parts) >= 3 {
			dayTitle = parts[2]
		}
	}

	// Find or create year
	var year *OrgYear
	for _, y := range s.years {
		if y.Title == yearTitle {
			year = y
			break
		}
	}
	if year == nil {
		year = &OrgYear{Title: yearTitle}
		s.years = append(s.years, year)
		sort.Slice(s.years, func(i, j int) bool {
			return s.years[i].Title < s.years[j].Title
		})
	}

	// Find or create month
	var month *OrgMonth
	for _, mo := range year.Months {
		if mo.Title == monthTitle {
			month = mo
			break
		}
	}
	if month == nil {
		month = &OrgMonth{Title: monthTitle}
		year.Months = append(year.Months, month)
	}

	// Find or create day
	var day *OrgDay
	for _, d := range month.Days {
		if d.Title == dayTitle {
			day = d
			break
		}
	}
	if day == nil {
		day = &OrgDay{Title: dayTitle}
		month.Days = append(month.Days, day)
	}

	day.Messages = append(day.Messages, msg)

	return msg, s.saveLocked()
}

// DeleteMessage removes a message by UID.
func (s *OrgStore) DeleteMessage(uid uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, y := range s.years {
		for _, mo := range y.Months {
			for _, d := range mo.Days {
				for i, m := range d.Messages {
					if m.UID == uid {
						d.Messages = append(d.Messages[:i], d.Messages[i+1:]...)
						delete(s.uidMap, uid)
						return s.saveLocked()
					}
				}
			}
		}
	}
	return fmt.Errorf("message UID %d not found", uid)
}

// UpdateFlags sets flags on a message by UID.
func (s *OrgStore) UpdateFlags(uid uint32, flags []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg, ok := s.uidMap[uid]
	if !ok {
		return fmt.Errorf("UID %d not found", uid)
	}
	msg.Flags = flags
	msg.IsTODO = false
	for _, f := range flags {
		if f == "\\Flagged" {
			msg.IsTODO = true
		}
	}
	return s.saveLocked()
}

// CheckReload re-reads the file if it has changed on disk.
func (s *OrgStore) CheckReload() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Stat(s.filePath)
	if err != nil {
		return false
	}
	if info.ModTime().After(s.modTime) {
		oldValidity := s.uidValidity
		err := s.loadLocked()
		if err != nil {
			log.Printf("reload error: %v", err)
			return false
		}
		_ = oldValidity
		s.uidValidity = uint32(time.Now().Unix())
		return true
	}
	return false
}

func (s *OrgStore) UIDValidity() uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.uidValidity
}

// ---------------------------------------------------------------------------
// HTML stripping
// ---------------------------------------------------------------------------

var reHTMLTag = regexp.MustCompile(`<[^>]*>`)

func stripHTML(s string) string {
	s = reHTMLTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return s
}

// ---------------------------------------------------------------------------
// RFC 2822 message formatting
// ---------------------------------------------------------------------------

func formatRFC2822(msg *OrgMessage) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("From: orgmail@localhost\r\n"))
	sb.WriteString(fmt.Sprintf("To: user@localhost\r\n"))
	sb.WriteString(fmt.Sprintf("Date: %s\r\n", msg.Date.Format(time.RFC1123Z)))
	sb.WriteString(fmt.Sprintf("Subject: %s\r\n", msg.Subject))
	sb.WriteString(fmt.Sprintf("Message-ID: <org-%d@localhost>\r\n", msg.UID))
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	sb.WriteString("\r\n")
	// Body: convert \n to \r\n
	body := strings.ReplaceAll(msg.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	sb.WriteString(body)
	sb.WriteString("\r\n")
	return sb.String()
}

// ---------------------------------------------------------------------------
// IMAP Session
// ---------------------------------------------------------------------------

type IMAPState int

const (
	StateNotAuthenticated IMAPState = iota
	StateAuthenticated
	StateSelected
	StateLogout
)

type IMAPSession struct {
	conn           net.Conn
	reader         *bufio.Reader
	store          *OrgStore
	state          IMAPState
	selectedFolder string
	readonly       bool
}

func NewIMAPSession(conn net.Conn, store *OrgStore) *IMAPSession {
	return &IMAPSession{
		conn:   conn,
		reader: bufio.NewReader(conn),
		store:  store,
		state:  StateNotAuthenticated,
	}
}

func (s *IMAPSession) send(format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	log.Printf("S: %s", strings.TrimRight(line, "\r\n"))
	fmt.Fprint(s.conn, line+"\r\n")
}

func (s *IMAPSession) Run() {
	defer s.conn.Close()

	s.send("* OK [CAPABILITY IMAP4rev1] OrgMail IMAP server ready")

	for s.state != StateLogout {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("read error: %v", err)
			}
			return
		}
		line = strings.TrimRight(line, "\r\n")
		log.Printf("C: %s", line)

		if line == "" {
			continue
		}

		s.handleCommand(line)
	}
}

func (s *IMAPSession) handleCommand(line string) {
	// Parse: tag SP command SP args
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		s.send("* BAD Invalid command")
		return
	}

	tag := parts[0]
	cmd := strings.ToUpper(parts[1])
	var args string
	if len(parts) > 2 {
		args = parts[2]
	}

	switch cmd {
	case "CAPABILITY":
		s.send("* CAPABILITY IMAP4rev1 AUTH=PLAIN")
		s.send("%s OK CAPABILITY completed", tag)

	case "NOOP":
		// Check for file changes
		if s.store.CheckReload() && s.state == StateSelected {
			msgs := s.store.MessagesInFolder(s.selectedFolder)
			s.send("* %d EXISTS", len(msgs))
		}
		s.send("%s OK NOOP completed", tag)

	case "LOGOUT":
		s.send("* BYE OrgMail IMAP server logging out")
		s.send("%s OK LOGOUT completed", tag)
		s.state = StateLogout

	case "LOGIN":
		// Accept any credentials (local only, no security needed)
		s.state = StateAuthenticated
		s.send("%s OK LOGIN completed", tag)

	case "AUTHENTICATE":
		// Accept PLAIN auth
		if strings.ToUpper(args) == "PLAIN" {
			s.send("+")
			// Read the base64 credentials line
			_, err := s.reader.ReadString('\n')
			if err != nil {
				return
			}
			s.state = StateAuthenticated
			s.send("%s OK AUTHENTICATE completed", tag)
		} else {
			s.send("%s NO Unsupported auth mechanism", tag)
		}

	case "LIST":
		s.handleList(tag, args)

	case "LSUB":
		s.handleList(tag, args) // treat LSUB same as LIST

	case "SELECT", "EXAMINE":
		s.handleSelect(tag, args, cmd == "EXAMINE")

	case "STATUS":
		s.handleStatus(tag, args)

	case "FETCH":
		s.handleFetch(tag, args)

	case "UID":
		s.handleUID(tag, args)

	case "STORE":
		s.handleStore(tag, args)

	case "APPEND":
		s.handleAppend(tag, args)

	case "EXPUNGE":
		s.handleExpunge(tag)

	case "CLOSE":
		s.selectedFolder = ""
		s.state = StateAuthenticated
		s.send("%s OK CLOSE completed", tag)

	case "SEARCH":
		s.handleSearch(tag, args)

	case "CREATE", "DELETE", "RENAME", "SUBSCRIBE", "UNSUBSCRIBE":
		// Stub – not fully implemented
		s.send("%s OK %s completed", tag, cmd)

	case "CHECK":
		s.store.CheckReload()
		s.send("%s OK CHECK completed", tag)

	case "COPY":
		s.send("%s NO COPY not implemented", tag)

	case "IDLE":
		s.handleIdle(tag)

	default:
		s.send("%s BAD Unknown command %s", tag, cmd)
	}
}

// ---------------------------------------------------------------------------
// LIST
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleList(tag, args string) {
	// Parse reference and mailbox pattern
	ref, pattern := parseListArgs(args)
	_ = ref

	folders := s.store.FolderPaths()

	if pattern == "" {
		// Return hierarchy delimiter
		s.send(`* LIST (\Noselect) "/" ""`)
		s.send("%s OK LIST completed", tag)
		return
	}

	// Convert IMAP wildcards to regex
	regexPattern := "^" + strings.ReplaceAll(strings.ReplaceAll(
		regexp.QuoteMeta(pattern), `\*`, ".*"), `\%`, "[^/]*") + "$"
	re, err := regexp.Compile(regexPattern)
	if err != nil {
		s.send("%s BAD Invalid pattern", tag)
		return
	}

	for _, f := range folders {
		if re.MatchString(f) {
			attrs := ""
			// Check if this folder has children
			hasChildren := false
			for _, f2 := range folders {
				if strings.HasPrefix(f2, f+"/") {
					hasChildren = true
					break
				}
			}
			if hasChildren {
				attrs = `\HasChildren`
			} else {
				attrs = `\HasNoChildren`
			}
			s.send(`* LIST (%s) "/" "%s"`, attrs, f)
		}
	}
	s.send("%s OK LIST completed", tag)
}

func parseListArgs(args string) (string, string) {
	// Args can be: "ref" "pattern" or ref pattern
	parts := parseQuotedStrings(args)
	if len(parts) >= 2 {
		return parts[0], parts[1]
	}
	if len(parts) == 1 {
		return "", parts[0]
	}
	return "", "*"
}

func parseQuotedStrings(s string) []string {
	var result []string
	s = strings.TrimSpace(s)
	for s != "" {
		s = strings.TrimSpace(s)
		if s == "" {
			break
		}
		if s[0] == '"' {
			// Find closing quote
			end := strings.Index(s[1:], "\"")
			if end == -1 {
				result = append(result, s[1:])
				break
			}
			result = append(result, s[1:end+1])
			s = s[end+2:]
		} else {
			end := strings.IndexByte(s, ' ')
			if end == -1 {
				result = append(result, s)
				break
			}
			result = append(result, s[:end])
			s = s[end+1:]
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// SELECT / EXAMINE
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleSelect(tag, args string, readonly bool) {
	folder := strings.Trim(strings.TrimSpace(args), "\"")

	msgs := s.store.MessagesInFolder(folder)
	if msgs == nil && !strings.EqualFold(folder, "INBOX") {
		// Check if it's a valid parent folder
		found := false
		for _, f := range s.store.FolderPaths() {
			if f == folder {
				found = true
				break
			}
		}
		if !found {
			s.send("%s NO Mailbox not found", tag)
			return
		}
		msgs = []*OrgMessage{} // empty folder
	}

	s.selectedFolder = folder
	s.readonly = readonly
	s.state = StateSelected

	s.send("* %d EXISTS", len(msgs))
	s.send("* 0 RECENT")
	s.send("* OK [UIDVALIDITY %d] UIDs valid", s.store.UIDValidity())

	nextUID := uint32(1)
	if len(msgs) > 0 {
		nextUID = msgs[len(msgs)-1].UID + 1
	}
	s.send("* OK [UIDNEXT %d] Predicted next UID", nextUID)
	s.send("* FLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft)")
	s.send("* OK [PERMANENTFLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft \\*)] Limited")

	if readonly {
		s.send("%s OK [READ-ONLY] EXAMINE completed", tag)
	} else {
		s.send("%s OK [READ-WRITE] SELECT completed", tag)
	}
}

// ---------------------------------------------------------------------------
// STATUS
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleStatus(tag, args string) {
	// STATUS "folder" (MESSAGES RECENT UIDNEXT UIDVALIDITY UNSEEN)
	parts := parseQuotedStrings(args)
	if len(parts) < 1 {
		s.send("%s BAD Invalid STATUS arguments", tag)
		return
	}
	folder := parts[0]
	msgs := s.store.MessagesInFolder(folder)
	count := len(msgs)

	unseen := 0
	for _, m := range msgs {
		seen := false
		for _, f := range m.Flags {
			if f == "\\Seen" {
				seen = true
				break
			}
		}
		if !seen {
			unseen++
		}
	}

	nextUID := uint32(1)
	if count > 0 {
		nextUID = msgs[count-1].UID + 1
	}

	s.send("* STATUS \"%s\" (MESSAGES %d RECENT 0 UIDNEXT %d UIDVALIDITY %d UNSEEN %d)",
		folder, count, nextUID, s.store.UIDValidity(), unseen)
	s.send("%s OK STATUS completed", tag)
}

// ---------------------------------------------------------------------------
// FETCH
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleFetch(tag, args string) {
	if s.state != StateSelected {
		s.send("%s BAD Not in selected state", tag)
		return
	}

	msgs := s.store.MessagesInFolder(s.selectedFolder)

	// Parse sequence set and data items
	seqPart, dataPart := splitFetchArgs(args)
	seqNums := parseSequenceSet(seqPart, len(msgs))

	for _, seq := range seqNums {
		if seq < 1 || seq > len(msgs) {
			continue
		}
		msg := msgs[seq-1]
		response := s.buildFetchResponse(seq, msg, dataPart)
		s.send("* %d FETCH (%s)", seq, response)
	}

	s.send("%s OK FETCH completed", tag)
}

// ---------------------------------------------------------------------------
// UID command
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleUID(tag, args string) {
	if s.state != StateSelected {
		s.send("%s BAD Not in selected state", tag)
		return
	}

	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		s.send("%s BAD Invalid UID command", tag)
		return
	}

	subCmd := strings.ToUpper(parts[0])
	subArgs := parts[1]

	switch subCmd {
	case "FETCH":
		s.handleUIDFetch(tag, subArgs)
	case "SEARCH":
		s.handleUIDSearch(tag, subArgs)
	case "STORE":
		s.handleUIDStore(tag, subArgs)
	case "COPY":
		s.send("%s NO COPY not implemented", tag)
	default:
		s.send("%s BAD Unknown UID subcommand", tag)
	}
}

func (s *IMAPSession) handleUIDFetch(tag, args string) {
	msgs := s.store.MessagesInFolder(s.selectedFolder)

	seqPart, dataPart := splitFetchArgs(args)
	uids := parseUIDSet(seqPart, msgs)

	for _, uid := range uids {
		// Find sequence number and message
		for seq, msg := range msgs {
			if msg.UID == uid {
				response := s.buildFetchResponse(seq+1, msg, dataPart)
				s.send("* %d FETCH (UID %d %s)", seq+1, msg.UID, response)
				break
			}
		}
	}

	s.send("%s OK UID FETCH completed", tag)
}

func (s *IMAPSession) handleUIDSearch(tag, args string) {
	msgs := s.store.MessagesInFolder(s.selectedFolder)

	criteria := strings.ToUpper(strings.TrimSpace(args))

	var matchedUIDs []string

	for _, msg := range msgs {
		if matchesCriteria(msg, criteria) {
			matchedUIDs = append(matchedUIDs, fmt.Sprintf("%d", msg.UID))
		}
	}

	if len(matchedUIDs) > 0 {
		s.send("* SEARCH %s", strings.Join(matchedUIDs, " "))
	} else {
		s.send("* SEARCH")
	}
	s.send("%s OK UID SEARCH completed", tag)
}

func (s *IMAPSession) handleUIDStore(tag, args string) {
	msgs := s.store.MessagesInFolder(s.selectedFolder)

	// Parse: uid-set +FLAGS (\Seen) etc
	parts := strings.SplitN(args, " ", 3)
	if len(parts) < 3 {
		s.send("%s BAD Invalid UID STORE arguments", tag)
		return
	}

	uids := parseUIDSet(parts[0], msgs)
	action := strings.ToUpper(parts[1])
	flagsStr := parts[2]
	newFlags := parseFlagList(flagsStr)

	for _, uid := range uids {
		for seq, msg := range msgs {
			if msg.UID == uid {
				switch {
				case strings.HasPrefix(action, "+FLAGS"):
					for _, f := range newFlags {
						if !containsFlag(msg.Flags, f) {
							msg.Flags = append(msg.Flags, f)
						}
					}
				case strings.HasPrefix(action, "-FLAGS"):
					var remaining []string
					for _, f := range msg.Flags {
						if !containsFlag(newFlags, f) {
							remaining = append(remaining, f)
						}
					}
					msg.Flags = remaining
				case action == "FLAGS" || action == "FLAGS.SILENT":
					msg.Flags = newFlags
				}

				msg.IsTODO = containsFlag(msg.Flags, "\\Flagged")

				if !strings.HasSuffix(action, ".SILENT") {
					s.send("* %d FETCH (UID %d FLAGS (%s))", seq+1, msg.UID, strings.Join(msg.Flags, " "))
				}

				s.store.UpdateFlags(uid, msg.Flags)
				break
			}
		}
	}

	s.send("%s OK UID STORE completed", tag)
}

// ---------------------------------------------------------------------------
// STORE
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleStore(tag, args string) {
	if s.state != StateSelected {
		s.send("%s BAD Not in selected state", tag)
		return
	}

	msgs := s.store.MessagesInFolder(s.selectedFolder)

	parts := strings.SplitN(args, " ", 3)
	if len(parts) < 3 {
		s.send("%s BAD Invalid STORE arguments", tag)
		return
	}

	seqNums := parseSequenceSet(parts[0], len(msgs))
	action := strings.ToUpper(parts[1])
	newFlags := parseFlagList(parts[2])

	for _, seq := range seqNums {
		if seq < 1 || seq > len(msgs) {
			continue
		}
		msg := msgs[seq-1]

		switch {
		case strings.HasPrefix(action, "+FLAGS"):
			for _, f := range newFlags {
				if !containsFlag(msg.Flags, f) {
					msg.Flags = append(msg.Flags, f)
				}
			}
		case strings.HasPrefix(action, "-FLAGS"):
			var remaining []string
			for _, f := range msg.Flags {
				if !containsFlag(newFlags, f) {
					remaining = append(remaining, f)
				}
			}
			msg.Flags = remaining
		case action == "FLAGS" || action == "FLAGS.SILENT":
			msg.Flags = newFlags
		}

		msg.IsTODO = containsFlag(msg.Flags, "\\Flagged")
		s.store.UpdateFlags(msg.UID, msg.Flags)

		if !strings.HasSuffix(action, ".SILENT") {
			s.send("* %d FETCH (FLAGS (%s))", seq, strings.Join(msg.Flags, " "))
		}
	}

	s.send("%s OK STORE completed", tag)
}

// ---------------------------------------------------------------------------
// SEARCH
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleSearch(tag, args string) {
	if s.state != StateSelected {
		s.send("%s BAD Not in selected state", tag)
		return
	}

	msgs := s.store.MessagesInFolder(s.selectedFolder)
	criteria := strings.ToUpper(strings.TrimSpace(args))

	var matchedSeqs []string
	for i, msg := range msgs {
		if matchesCriteria(msg, criteria) {
			matchedSeqs = append(matchedSeqs, fmt.Sprintf("%d", i+1))
		}
	}

	if len(matchedSeqs) > 0 {
		s.send("* SEARCH %s", strings.Join(matchedSeqs, " "))
	} else {
		s.send("* SEARCH")
	}
	s.send("%s OK SEARCH completed", tag)
}

func matchesCriteria(msg *OrgMessage, criteria string) bool {
	// Simple criteria matching
	if criteria == "ALL" || criteria == "" {
		return true
	}
	if strings.Contains(criteria, "UNSEEN") {
		return !containsFlag(msg.Flags, "\\Seen")
	}
	if strings.Contains(criteria, "SEEN") {
		return containsFlag(msg.Flags, "\\Seen")
	}
	if strings.Contains(criteria, "FLAGGED") {
		return containsFlag(msg.Flags, "\\Flagged")
	}
	if strings.Contains(criteria, "DELETED") {
		return containsFlag(msg.Flags, "\\Deleted")
	}
	// Default: match all
	return true
}

// ---------------------------------------------------------------------------
// APPEND
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleAppend(tag, args string) {
	// APPEND "folder" (\Flags) {size}
	// or APPEND "folder" {size}
	parts := parseQuotedStrings(args)
	folder := "INBOX"
	if len(parts) >= 1 {
		folder = parts[0]
	}

	// Find the literal size {NNN}
	idx := strings.LastIndex(args, "{")
	if idx == -1 {
		s.send("%s BAD Missing literal size", tag)
		return
	}
	sizeStr := args[idx+1 : len(args)-1]
	size, err := strconv.Atoi(sizeStr)
	if err != nil {
		s.send("%s BAD Invalid literal size", tag)
		return
	}

	// Parse flags between folder and literal
	flagStr := ""
	afterFolder := args[len(parts[0])+2:] // skip past folder + quotes
	if fIdx := strings.Index(afterFolder, "("); fIdx != -1 {
		fEnd := strings.Index(afterFolder, ")")
		if fEnd != -1 {
			flagStr = afterFolder[fIdx+1 : fEnd]
		}
	}
	flags := parseFlagList("(" + flagStr + ")")

	// Send continuation
	s.send("+ Ready for literal data")

	// Read exactly `size` bytes
	buf := make([]byte, size)
	_, err = io.ReadFull(s.reader, buf)
	if err != nil {
		s.send("%s BAD Failed to read literal data", tag)
		return
	}

	// Read the trailing CRLF
	s.reader.ReadString('\n')

	// Parse the RFC2822 message
	msgContent := string(buf)
	subject, body, date := parseRFC2822(msgContent)

	_, err = s.store.AppendMessage(folder, date, subject, body, flags)
	if err != nil {
		s.send("%s NO APPEND failed: %v", tag, err)
		return
	}

	s.send("%s OK APPEND completed", tag)
}

func parseRFC2822(raw string) (subject, body string, date time.Time) {
	date = time.Now()

	// Split headers and body
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	if len(parts) < 2 {
		parts = strings.SplitN(raw, "\n\n", 2)
	}

	headers := ""
	if len(parts) >= 1 {
		headers = parts[0]
	}
	if len(parts) >= 2 {
		body = parts[1]
	}

	// Parse headers
	for _, line := range strings.Split(headers, "\n") {
		line = strings.TrimRight(line, "\r")
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "subject:") {
			subject = strings.TrimSpace(line[8:])
		}
		if strings.HasPrefix(lower, "date:") {
			dateStr := strings.TrimSpace(line[5:])
			for _, layout := range []string{
				time.RFC1123Z,
				time.RFC1123,
				time.RFC822Z,
				time.RFC822,
				"Mon, 2 Jan 2006 15:04:05 -0700",
				"2 Jan 2006 15:04:05 -0700",
			} {
				if t, err := time.Parse(layout, dateStr); err == nil {
					date = t
					break
				}
			}
		}
	}

	body = stripHTML(body)
	body = strings.TrimRight(body, "\r\n ")

	return
}

// ---------------------------------------------------------------------------
// EXPUNGE
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleExpunge(tag string) {
	if s.state != StateSelected {
		s.send("%s BAD Not in selected state", tag)
		return
	}

	msgs := s.store.MessagesInFolder(s.selectedFolder)

	// Find messages marked \Deleted and remove them
	var toDelete []uint32
	for seq, msg := range msgs {
		if containsFlag(msg.Flags, "\\Deleted") {
			toDelete = append(toDelete, msg.UID)
			s.send("* %d EXPUNGE", seq+1)
		}
	}

	for _, uid := range toDelete {
		s.store.DeleteMessage(uid)
	}

	s.send("%s OK EXPUNGE completed", tag)
}

// ---------------------------------------------------------------------------
// IDLE
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleIdle(tag string) {
	s.send("+ idling")

	// Poll for changes every 2 seconds or until client sends DONE
	doneCh := make(chan struct{})

	go func() {
		for {
			line, err := s.reader.ReadString('\n')
			if err != nil {
				close(doneCh)
				return
			}
			if strings.TrimSpace(strings.ToUpper(line)) == "DONE" {
				close(doneCh)
				return
			}
		}
	}()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-doneCh:
			s.send("%s OK IDLE terminated", tag)
			return
		case <-ticker.C:
			if s.store.CheckReload() && s.state == StateSelected {
				msgs := s.store.MessagesInFolder(s.selectedFolder)
				s.send("* %d EXISTS", len(msgs))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// FETCH response builder
// ---------------------------------------------------------------------------

func splitFetchArgs(args string) (string, string) {
	// Sequence set is first token, rest is data items
	idx := strings.IndexByte(args, ' ')
	if idx == -1 {
		return args, "ALL"
	}
	return args[:idx], args[idx+1:]
}

func (s *IMAPSession) buildFetchResponse(seq int, msg *OrgMessage, dataItems string) string {
	var parts []string

	items := strings.ToUpper(dataItems)

	// Expand macros
	if items == "ALL" || items == "(ALL)" {
		items = "FLAGS INTERNALDATE RFC822.SIZE ENVELOPE"
	}
	if items == "FULL" || items == "(FULL)" {
		items = "FLAGS INTERNALDATE RFC822.SIZE ENVELOPE BODY"
	}
	if items == "FAST" || items == "(FAST)" {
		items = "FLAGS INTERNALDATE RFC822.SIZE"
	}

	// Remove outer parens
	items = strings.Trim(items, "()")

	rfc2822 := formatRFC2822(msg)

	if strings.Contains(items, "FLAGS") {
		parts = append(parts, fmt.Sprintf("FLAGS (%s)", strings.Join(msg.Flags, " ")))
	}

	if strings.Contains(items, "INTERNALDATE") {
		parts = append(parts, fmt.Sprintf(`INTERNALDATE "%s"`, msg.Date.Format("02-Jan-2006 15:04:05 -0700")))
	}

	if strings.Contains(items, "RFC822.SIZE") {
		parts = append(parts, fmt.Sprintf("RFC822.SIZE %d", len(rfc2822)))
	}

	if strings.Contains(items, "RFC822.HEADER") {
		headerEnd := strings.Index(rfc2822, "\r\n\r\n")
		header := rfc2822
		if headerEnd != -1 {
			header = rfc2822[:headerEnd+4]
		}
		parts = append(parts, fmt.Sprintf("RFC822.HEADER {%d}\r\n%s", len(header), header))
	}

	if strings.Contains(items, "BODY.PEEK[HEADER.FIELDS") || strings.Contains(items, "BODY[HEADER.FIELDS") {
		// Extract wanted fields
		headerEnd := strings.Index(rfc2822, "\r\n\r\n")
		header := rfc2822
		if headerEnd != -1 {
			header = rfc2822[:headerEnd+4]
		}

		// Figure out which fields are requested
		fieldStart := strings.Index(items, "(")
		fieldEnd := strings.Index(items, ")")
		requestedFields := ""
		if fieldStart != -1 && fieldEnd != -1 {
			requestedFields = items[fieldStart+1 : fieldEnd]
		}

		filteredHeader := filterHeaders(header, requestedFields)

		// Determine the exact item name to use in response
		itemName := "BODY[HEADER.FIELDS (" + requestedFields + ")]"
		parts = append(parts, fmt.Sprintf("%s {%d}\r\n%s", itemName, len(filteredHeader), filteredHeader))
	} else if strings.Contains(items, "BODY.PEEK[]") || strings.Contains(items, "BODY[]") {
		parts = append(parts, fmt.Sprintf("BODY[] {%d}\r\n%s", len(rfc2822), rfc2822))
	} else if strings.Contains(items, "BODY.PEEK[HEADER]") || strings.Contains(items, "BODY[HEADER]") {
		headerEnd := strings.Index(rfc2822, "\r\n\r\n")
		header := rfc2822
		if headerEnd != -1 {
			header = rfc2822[:headerEnd+4]
		}
		parts = append(parts, fmt.Sprintf("BODY[HEADER] {%d}\r\n%s", len(header), header))
	} else if strings.Contains(items, "BODY.PEEK[TEXT]") || strings.Contains(items, "BODY[TEXT]") {
		headerEnd := strings.Index(rfc2822, "\r\n\r\n")
		text := ""
		if headerEnd != -1 {
			text = rfc2822[headerEnd+4:]
		}
		parts = append(parts, fmt.Sprintf("BODY[TEXT] {%d}\r\n%s", len(text), text))
	} else if strings.Contains(items, "RFC822") && !strings.Contains(items, "RFC822.SIZE") && !strings.Contains(items, "RFC822.HEADER") {
		parts = append(parts, fmt.Sprintf("RFC822 {%d}\r\n%s", len(rfc2822), rfc2822))
	}

	if strings.Contains(items, "ENVELOPE") {
		env := fmt.Sprintf(`(%s "%s" (("orgmail" NIL "orgmail" "localhost")) (("orgmail" NIL "orgmail" "localhost")) (("orgmail" NIL "orgmail" "localhost")) (("user" NIL "user" "localhost")) NIL NIL NIL "<org-%d@localhost>")`,
			`"`+msg.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700")+`"`,
			escapeIMAPString(msg.Subject),
			msg.UID)
		parts = append(parts, "ENVELOPE "+env)
	}

	if strings.Contains(items, "BODYSTRUCTURE") {
		parts = append(parts, fmt.Sprintf(`BODYSTRUCTURE ("TEXT" "PLAIN" ("CHARSET" "UTF-8") NIL NIL "7BIT" %d %d)`,
			len(msg.Body), strings.Count(msg.Body, "\n")+1))
	}

	if strings.Contains(items, "UID") {
		parts = append(parts, fmt.Sprintf("UID %d", msg.UID))
	}

	return strings.Join(parts, " ")
}

func filterHeaders(header string, fields string) string {
	wanted := make(map[string]bool)
	for _, f := range strings.Fields(fields) {
		wanted[strings.ToUpper(strings.TrimSpace(f))] = true
	}

	var result strings.Builder
	lines := strings.Split(header, "\r\n")
	include := false
	for _, line := range lines {
		if line == "" {
			result.WriteString("\r\n")
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			// New header
			colonIdx := strings.IndexByte(line, ':')
			if colonIdx != -1 {
				name := strings.ToUpper(strings.TrimSpace(line[:colonIdx]))
				include = wanted[name]
			} else {
				include = false
			}
		}
		if include {
			result.WriteString(line)
			result.WriteString("\r\n")
		}
	}
	result.WriteString("\r\n")
	return result.String()
}

func escapeIMAPString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

// ---------------------------------------------------------------------------
// Sequence/UID set parsing
// ---------------------------------------------------------------------------

func parseSequenceSet(set string, total int) []int {
	var result []int
	set = strings.TrimSpace(set)
	for _, part := range strings.Split(set, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, ":") {
			rangeParts := strings.SplitN(part, ":", 2)
			start := resolveSeqNum(rangeParts[0], total)
			end := resolveSeqNum(rangeParts[1], total)
			if start > end {
				start, end = end, start
			}
			for i := start; i <= end; i++ {
				result = append(result, i)
			}
		} else {
			n := resolveSeqNum(part, total)
			if n > 0 {
				result = append(result, n)
			}
		}
	}
	return result
}

func resolveSeqNum(s string, total int) int {
	s = strings.TrimSpace(s)
	if s == "*" {
		return total
	}
	n, _ := strconv.Atoi(s)
	return n
}

func parseUIDSet(set string, msgs []*OrgMessage) []uint32 {
	var result []uint32
	if len(msgs) == 0 {
		return result
	}

	maxUID := msgs[len(msgs)-1].UID

	for _, part := range strings.Split(set, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, ":") {
			rangeParts := strings.SplitN(part, ":", 2)
			start := resolveUID(rangeParts[0], maxUID)
			end := resolveUID(rangeParts[1], maxUID)
			if start > end {
				start, end = end, start
			}
			for _, m := range msgs {
				if m.UID >= start && m.UID <= end {
					result = append(result, m.UID)
				}
			}
		} else {
			uid := resolveUID(part, maxUID)
			for _, m := range msgs {
				if m.UID == uid {
					result = append(result, m.UID)
					break
				}
			}
		}
	}
	return result
}

func resolveUID(s string, maxUID uint32) uint32 {
	s = strings.TrimSpace(s)
	if s == "*" {
		return maxUID
	}
	n, _ := strconv.ParseUint(s, 10, 32)
	return uint32(n)
}

// ---------------------------------------------------------------------------
// Flag parsing helpers
// ---------------------------------------------------------------------------

func parseFlagList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "()")
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var flags []string
	for _, f := range strings.Fields(s) {
		f = strings.TrimSpace(f)
		if f != "" {
			flags = append(flags, f)
		}
	}
	return flags
}

func containsFlag(flags []string, flag string) bool {
	flag = strings.ToUpper(flag)
	for _, f := range flags {
		if strings.ToUpper(f) == flag {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	orgFile := "mail.org"
	listenAddr := "127.0.0.1:1143"

	if len(os.Args) > 1 {
		orgFile = os.Args[1]
	}
	if len(os.Args) > 2 {
		listenAddr = os.Args[2]
	}

	store := NewOrgStore(orgFile)
	if err := store.Load(); err != nil {
		log.Fatalf("Failed to load org file: %v", err)
	}

	msgs := store.allMessages()
	log.Printf("Loaded %d messages from %s", len(msgs), orgFile)

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", listenAddr, err)
	}
	defer listener.Close()

	log.Printf("IMAP server listening on %s (org file: %s)", listenAddr, orgFile)

	// File watcher goroutine
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			store.CheckReload()
		}
	}()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down...")
		listener.Close()
		os.Exit(0)
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		log.Printf("New connection from %s", conn.RemoteAddr())
		session := NewIMAPSession(conn, store)
		go session.Run()
	}
}
