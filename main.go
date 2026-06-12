// main.go
package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
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

type OrgMessage struct {
	UID     uint32
	Date    time.Time
	Subject string
	Body    string
	Flags   []string
	IsTODO  bool
}

type OrgDay struct {
	Title    string
	Messages []*OrgMessage
}

type OrgMonth struct {
	Title string
	Days  []*OrgDay
}

type OrgYear struct {
	Title  string
	Months []*OrgMonth
}

type OrgStore struct {
	mu          sync.RWMutex
	filePath    string
	years       []*OrgYear
	nextUID     uint32
	modTime     time.Time
	uidValidity uint32
	uidMap      map[uint32]*OrgMessage
	lastHash    [16]byte
	localLoc    *time.Location
}

func NewOrgStore(path string) *OrgStore {
	return &OrgStore{
		filePath:    path,
		nextUID:     1,
		uidValidity: uint32(time.Now().Unix()),
		uidMap:      make(map[uint32]*OrgMessage),
		localLoc:    time.Now().Location(),
	}
}

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
		return nil
	}
	s.lastHash = newHash

	info, _ := os.Stat(s.filePath)
	if info != nil {
		s.modTime = info.ModTime()
	}

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

	// [2026-06-11 Thu 14:01] subject
	// TODO [2026-06-11 Thu 14:01] subject
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
			body := strings.Join(bodyLines, "\n")
			// Remove leading empty lines
			body = strings.TrimLeft(body, "\n\r ")
			body = strings.TrimRight(body, "\n\r ")
			curMsg.Body = body
			curDay.Messages = append(curDay.Messages, curMsg)
			s.uidMap[curMsg.UID] = curMsg
		}
		curMsg = nil
		bodyLines = nil
	}

	for _, line := range lines {
		if m := reH4.FindStringSubmatch(line); m != nil {
			flushMsg()
			rest := strings.TrimSpace(m[1])
			msg := &OrgMessage{
				UID: s.nextUID,
			}
			s.nextUID++

			if dm := reH4Detail.FindStringSubmatch(rest); dm != nil {
				msg.IsTODO = strings.TrimSpace(dm[1]) == "TODO"
				if msg.IsTODO {
					msg.Flags = append(msg.Flags, "\\Flagged")
				}
				t, err := time.ParseInLocation("2006-01-02 Mon 15:04", dm[2], s.localLoc)
				if err != nil {
					t = time.Now().In(s.localLoc)
				}
				msg.Date = t
				msg.Subject = dm[3]
			} else {
				msg.Subject = rest
				msg.Date = time.Now().In(s.localLoc)
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

		if curMsg != nil {
			bodyLines = append(bodyLines, line)
		}
	}
	flushMsg()
}

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
		fmt.Fprintf(&sb, "* %s\n", y.Title)
		for _, mo := range y.Months {
			fmt.Fprintf(&sb, "** %s\n", mo.Title)
			for _, d := range mo.Days {
				fmt.Fprintf(&sb, "*** %s\n", d.Title)
				for _, msg := range d.Messages {
					sb.WriteString("**** ")
					if msg.IsTODO {
						sb.WriteString("TODO ")
					}
					fmt.Fprintf(&sb, "[%s] %s\n",
						msg.Date.In(s.localLoc).Format("2006-01-02 Mon 15:04"),
						msg.Subject)
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
// Folder operations
// ---------------------------------------------------------------------------

// FolderPaths returns flat IMAP-style folder names using "/" separator.
// Only *** (day-level) folders contain messages; year and month folders
// exist solely as hierarchy containers.
func (s *OrgStore) FolderPaths() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var paths []string
	paths = append(paths, "INBOX")
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

// MessagesInFolder returns messages for a folder.
// Only the deepest matching level returns messages — no aggregation upward.
// INBOX is special: it returns ALL messages (for clients that expect it).
func (s *OrgStore) MessagesInFolder(folder string) []*OrgMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if strings.EqualFold(folder, "INBOX") {
		return s.allMessages()
	}

	parts := strings.Split(folder, "/")
	switch len(parts) {
	case 1:
		// Year-level folder: only return messages that sit directly
		// in **** under a * heading with no ** or *** intermediary.
		// In our model **** always lives under ***, so year-level
		// folders contain no messages themselves.
		return nil
	case 2:
		// Month-level: same reasoning — no direct messages.
		return nil
	case 3:
		// Day-level: this is where messages live.
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
		for _, mo := range y.Months {
			for _, d := range mo.Days {
				msgs = append(msgs, d.Messages...)
			}
		}
	}
	return msgs
}

// AppendMessage adds a message. HTML body is converted to markdown.
// Subject is decoded to UTF-8.
func (s *OrgStore) AppendMessage(folder string, date time.Time, subject, body string, flags []string) (*OrgMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Ensure local timezone
	date = date.In(s.localLoc)

	// Ensure valid UTF-8
	if !utf8.ValidString(body) {
		body = strings.ToValidUTF8(body, "?")
	}
	if !utf8.ValidString(subject) {
		subject = strings.ToValidUTF8(subject, "?")
	}

	// Remove leading blank lines from body
	body = trimLeadingEmptyLines(body)
	body = strings.TrimRight(body, "\n\r ")

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

	// Determine target headings from date
	yearTitle := fmt.Sprintf("%d", date.Year())
	monthTitle := date.Format("2006-01") + " " + date.Format("January")
	dayTitle := date.Format("2006-01-02") + " " + date.Format("Monday")

	// Override if a specific 3-level folder was given
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

	year := s.findOrCreateYear(yearTitle)
	month := s.findOrCreateMonth(year, monthTitle)
	day := s.findOrCreateDay(month, dayTitle)

	day.Messages = append(day.Messages, msg)

	return msg, s.saveLocked()
}

func (s *OrgStore) findOrCreateYear(title string) *OrgYear {
	for _, y := range s.years {
		if y.Title == title {
			return y
		}
	}
	y := &OrgYear{Title: title}
	s.years = append(s.years, y)
	sort.Slice(s.years, func(i, j int) bool {
		return s.years[i].Title < s.years[j].Title
	})
	return y
}

func (s *OrgStore) findOrCreateMonth(year *OrgYear, title string) *OrgMonth {
	for _, mo := range year.Months {
		if mo.Title == title {
			return mo
		}
	}
	mo := &OrgMonth{Title: title}
	year.Months = append(year.Months, mo)
	return mo
}

func (s *OrgStore) findOrCreateDay(month *OrgMonth, title string) *OrgDay {
	for _, d := range month.Days {
		if d.Title == title {
			return d
		}
	}
	d := &OrgDay{Title: title}
	month.Days = append(month.Days, d)
	return d
}

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

func (s *OrgStore) UpdateFlags(uid uint32, flags []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg, ok := s.uidMap[uid]
	if !ok {
		return fmt.Errorf("UID %d not found", uid)
	}
	msg.Flags = flags
	msg.IsTODO = containsFlag(flags, "\\Flagged")
	return s.saveLocked()
}

func (s *OrgStore) CheckReload() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Stat(s.filePath)
	if err != nil {
		return false
	}
	if info.ModTime().After(s.modTime) {
		err := s.loadLocked()
		if err != nil {
			log.Printf("reload error: %v", err)
			return false
		}
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
// HTML to Markdown conversion
// ---------------------------------------------------------------------------

func htmlToMarkdown(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}

	r := s

	// Block-level elements first (before we strip tags)
	// <br> / <br/>
	r = regexp.MustCompile(`(?i)<br\s*/?\s*>`).ReplaceAllString(r, "\n")
	// <p>...</p>
	r = regexp.MustCompile(`(?i)<p[^>]*>`).ReplaceAllString(r, "\n\n")
	r = regexp.MustCompile(`(?i)</p>`).ReplaceAllString(r, "\n")
	// <div>
	r = regexp.MustCompile(`(?i)<div[^>]*>`).ReplaceAllString(r, "\n")
	r = regexp.MustCompile(`(?i)</div>`).ReplaceAllString(r, "\n")
	// <hr>
	r = regexp.MustCompile(`(?i)<hr\s*/?\s*>`).ReplaceAllString(r, "\n---\n")

	// Headings: <h1>...<h6>
	for i := 6; i >= 1; i-- {
		prefix := strings.Repeat("#", i)
		re := regexp.MustCompile(fmt.Sprintf(`(?i)<h%d[^>]*>(.*?)</h%d>`, i, i))
		r = re.ReplaceAllString(r, "\n"+prefix+" $1\n")
	}

	// Bold: <b>, <strong>
	r = regexp.MustCompile(`(?i)<(?:b|strong)[^>]*>(.*?)</(?:b|strong)>`).ReplaceAllString(r, "**$1**")
	// Italic: <i>, <em>
	r = regexp.MustCompile(`(?i)<(?:i|em)[^>]*>(.*?)</(?:i|em)>`).ReplaceAllString(r, "*$1*")
	// Code: <code>
	r = regexp.MustCompile(`(?i)<code[^>]*>(.*?)</code>`).ReplaceAllString(r, "`$1`")
	// Pre: <pre>
	r = regexp.MustCompile(`(?i)<pre[^>]*>(.*?)</pre>`).ReplaceAllString(r, "\n#+BEGIN_EXAMPLE\n$1\n#+END_EXAMPLE\n")

	// Links: <a href="url">text</a>
	r = regexp.MustCompile(`(?i)<a\s+[^>]*href\s*=\s*"([^"]*)"[^>]*>(.*?)</a>`).ReplaceAllString(r, "[[$1][$2]]")

	// Images: <img src="url" alt="text">
	r = regexp.MustCompile(`(?i)<img\s+[^>]*src\s*=\s*"([^"]*)"[^>]*/?\s*>`).ReplaceAllString(r, "[[$1]]")

	// Lists: <li>
	r = regexp.MustCompile(`(?i)<li[^>]*>`).ReplaceAllString(r, "\n- ")
	r = regexp.MustCompile(`(?i)</li>`).ReplaceAllString(r, "")
	// <ul>, <ol>
	r = regexp.MustCompile(`(?i)</?(?:ul|ol)[^>]*>`).ReplaceAllString(r, "\n")

	// Blockquote
	r = regexp.MustCompile(`(?i)<blockquote[^>]*>`).ReplaceAllString(r, "\n#+BEGIN_QUOTE\n")
	r = regexp.MustCompile(`(?i)</blockquote>`).ReplaceAllString(r, "\n#+END_QUOTE\n")

	// Table elements
	r = regexp.MustCompile(`(?i)<tr[^>]*>`).ReplaceAllString(r, "|")
	r = regexp.MustCompile(`(?i)</tr>`).ReplaceAllString(r, "|\n")
	r = regexp.MustCompile(`(?i)<t[dh][^>]*>`).ReplaceAllString(r, " ")
	r = regexp.MustCompile(`(?i)</t[dh]>`).ReplaceAllString(r, " |")
	r = regexp.MustCompile(`(?i)</?table[^>]*>`).ReplaceAllString(r, "\n")

	// Strip <style> and <script> blocks entirely
	r = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`).ReplaceAllString(r, "")
	r = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).ReplaceAllString(r, "")

	// Strip all remaining HTML tags
	r = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(r, "")

	// Decode HTML entities
	r = decodeHTMLEntities(r)

	// Clean up excessive blank lines
	r = regexp.MustCompile(`\n{3,}`).ReplaceAllString(r, "\n\n")

	// Remove leading empty lines
	r = trimLeadingEmptyLines(r)
	r = strings.TrimRight(r, "\n\r ")

	return r
}

func decodeHTMLEntities(s string) string {
	replacements := map[string]string{
		"&amp;":   "&",
		"&lt;":    "<",
		"&gt;":    ">",
		"&quot;":  "\"",
		"&apos;":  "'",
		"&#39;":   "'",
		"&nbsp;":  " ",
		"&mdash;": "—",
		"&ndash;": "–",
		"&laquo;": "«",
		"&raquo;": "»",
		"&copy;":  "©",
		"&reg;":   "®",
		"&trade;": "™",
		"&hellip;": "…",
	}
	r := s
	for entity, char := range replacements {
		r = strings.ReplaceAll(r, entity, char)
	}
	// Numeric entities &#NNN;
	r = regexp.MustCompile(`&#(\d+);`).ReplaceAllStringFunc(r, func(match string) string {
		numStr := match[2 : len(match)-1]
		n, err := strconv.Atoi(numStr)
		if err != nil || n < 0 || n > 0x10FFFF {
			return match
		}
		return string(rune(n))
	})
	// Hex entities &#xHHH;
	r = regexp.MustCompile(`(?i)&#x([0-9a-f]+);`).ReplaceAllStringFunc(r, func(match string) string {
		hexStr := match[3 : len(match)-1]
		n, err := strconv.ParseInt(hexStr, 16, 32)
		if err != nil || n < 0 || n > 0x10FFFF {
			return match
		}
		return string(rune(n))
	})
	return r
}

func trimLeadingEmptyLines(s string) string {
	lines := strings.Split(s, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start >= len(lines) {
		return ""
	}
	return strings.Join(lines[start:], "\n")
}

// ---------------------------------------------------------------------------
// MIME / RFC2822 parsing
// ---------------------------------------------------------------------------

// decodeMIMESubject decodes RFC 2047 encoded-words to plain UTF-8
func decodeMIMESubject(s string) string {
	dec := new(mime.WordDecoder)
	result, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return result
}

// parseRFC2822 extracts subject, body, and date from a raw RFC2822 message.
// HTML parts are converted to markdown. Body is decoded from
// base64/quoted-printable to plain UTF-8. Subject is decoded to UTF-8.
func parseRFC2822Full(raw string) (subject, body string, date time.Time) {
	localLoc := time.Now().Location()
	date = time.Now().In(localLoc)

	// Normalize line endings to \r\n for header parsing
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	// Split headers and body at first blank line
	headerBody := strings.SplitN(normalized, "\n\n", 2)

	headerSection := ""
	rawBody := ""
	if len(headerBody) >= 1 {
		headerSection = headerBody[0]
	}
	if len(headerBody) >= 2 {
		rawBody = headerBody[1]
	}

	// Parse headers (handle folded headers)
	headers := parseHeaders(headerSection)

	// Subject
	if v, ok := headers["subject"]; ok {
		subject = decodeMIMESubject(v)
	}

	// Date — parse and convert to local timezone
	if v, ok := headers["date"]; ok {
		for _, layout := range []string{
			time.RFC1123Z,
			time.RFC1123,
			time.RFC822Z,
			time.RFC822,
			"Mon, 2 Jan 2006 15:04:05 -0700",
			"Mon, 02 Jan 2006 15:04:05 -0700",
			"2 Jan 2006 15:04:05 -0700",
			"Mon, 2 Jan 2006 15:04:05 -0700 (MST)",
		} {
			if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
				date = t.In(localLoc)
				break
			}
		}
	}

	// Content-Type
	contentType := headers["content-type"]
	transferEncoding := strings.ToLower(strings.TrimSpace(headers["content-transfer-encoding"]))

	body = extractBody(rawBody, contentType, transferEncoding)

	// Final cleanup
	if !utf8.ValidString(body) {
		body = strings.ToValidUTF8(body, "?")
	}
	body = trimLeadingEmptyLines(body)
	body = strings.TrimRight(body, "\n\r ")

	if !utf8.ValidString(subject) {
		subject = strings.ToValidUTF8(subject, "?")
	}

	return
}

func parseHeaders(section string) map[string]string {
	headers := make(map[string]string)
	lines := strings.Split(section, "\n")

	var currentKey string
	var currentVal strings.Builder

	flush := func() {
		if currentKey != "" {
			headers[strings.ToLower(currentKey)] = strings.TrimSpace(currentVal.String())
		}
	}

	for _, line := range lines {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			// Continuation of previous header
			if currentKey != "" {
				currentVal.WriteString(" ")
				currentVal.WriteString(strings.TrimSpace(line))
			}
		} else {
			flush()
			colonIdx := strings.IndexByte(line, ':')
			if colonIdx > 0 {
				currentKey = line[:colonIdx]
				currentVal.Reset()
				currentVal.WriteString(strings.TrimSpace(line[colonIdx+1:]))
			} else {
				currentKey = ""
				currentVal.Reset()
			}
		}
	}
	flush()
	return headers
}

func extractBody(rawBody, contentType, transferEncoding string) string {
	if contentType == "" {
		contentType = "text/plain"
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// Fallback: treat as plain text
		return decodeTransferEncoding(rawBody, transferEncoding)
	}

	// Multipart
	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return decodeTransferEncoding(rawBody, transferEncoding)
		}
		return parseMultipart(rawBody, boundary)
	}

	decoded := decodeTransferEncoding(rawBody, transferEncoding)

	// Handle charset
	if charset, ok := params["charset"]; ok {
		decoded = ensureUTF8(decoded, charset)
	}

	if strings.HasPrefix(mediaType, "text/html") {
		return htmlToMarkdown(decoded)
	}

	return decoded
}

func parseMultipart(rawBody, boundary string) string {
	reader := multipart.NewReader(strings.NewReader(rawBody), boundary)

	var textPlain string
	var textHTML string

	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}

		partCT := part.Header.Get("Content-Type")
		partTE := strings.ToLower(strings.TrimSpace(part.Header.Get("Content-Transfer-Encoding")))

		partBytes, err := io.ReadAll(part)
		if err != nil {
			continue
		}
		partBody := string(partBytes)

		mediaType, params, _ := mime.ParseMediaType(partCT)

		if strings.HasPrefix(mediaType, "multipart/") {
			subBoundary := params["boundary"]
			if subBoundary != "" {
				result := parseMultipart(partBody, subBoundary)
				if result != "" {
					return result
				}
			}
			continue
		}

		decoded := decodeTransferEncoding(partBody, partTE)
		if charset, ok := params["charset"]; ok {
			decoded = ensureUTF8(decoded, charset)
		}

		if strings.HasPrefix(mediaType, "text/plain") {
			textPlain = decoded
		} else if strings.HasPrefix(mediaType, "text/html") {
			textHTML = decoded
		}
	}

	// Prefer plain text; fall back to HTML→markdown
	if textPlain != "" {
		return textPlain
	}
	if textHTML != "" {
		return htmlToMarkdown(textHTML)
	}
	return ""
}

func decodeTransferEncoding(s, encoding string) string {
	switch encoding {
	case "base64":
		// Remove whitespace from base64
		cleaned := strings.Join(strings.Fields(s), "")
		decoded, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			// Try with padding
			for len(cleaned)%4 != 0 {
				cleaned += "="
			}
			decoded, err = base64.StdEncoding.DecodeString(cleaned)
			if err != nil {
				return s
			}
		}
		return string(decoded)
	case "quoted-printable":
		reader := quotedprintable.NewReader(strings.NewReader(s))
		decoded, err := io.ReadAll(reader)
		if err != nil {
			return s
		}
		return string(decoded)
	default:
		return s
	}
}

// ensureUTF8 is a best-effort charset handler for common encodings.
// For a production system you'd use golang.org/x/text/encoding, but
// we keep zero dependencies here.
func ensureUTF8(s, charset string) string {
	charset = strings.ToLower(strings.TrimSpace(charset))
	switch charset {
	case "utf-8", "utf8", "us-ascii", "ascii", "":
		return s
	default:
		// Best effort: if it's valid UTF-8 already, keep it
		if utf8.ValidString(s) {
			return s
		}
		return strings.ToValidUTF8(s, "?")
	}
}

// ---------------------------------------------------------------------------
// RFC 2822 message formatting (for IMAP FETCH responses)
// ---------------------------------------------------------------------------

func formatRFC2822(msg *OrgMessage) string {
	var sb strings.Builder
	sb.WriteString("From: orgmail@localhost\r\n")
	sb.WriteString("To: user@localhost\r\n")
	sb.WriteString(fmt.Sprintf("Date: %s\r\n", msg.Date.Format(time.RFC1123Z)))
	// Encode subject for RFC2822 compatibility but keep it readable
	sb.WriteString(fmt.Sprintf("Subject: %s\r\n", encodeSubjectRFC2047(msg.Subject)))
	sb.WriteString(fmt.Sprintf("Message-ID: <org-%d@localhost>\r\n", msg.UID))
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	sb.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	sb.WriteString("\r\n")
	body := strings.ReplaceAll(msg.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	sb.WriteString(body)
	sb.WriteString("\r\n")
	return sb.String()
}

func encodeSubjectRFC2047(s string) string {
	// If pure ASCII, no encoding needed
	isASCII := true
	for _, r := range s {
		if r > 127 {
			isASCII = false
			break
		}
	}
	if isASCII {
		return s
	}
	// Use RFC 2047 UTF-8 B encoding
	encoded := base64.StdEncoding.EncodeToString([]byte(s))
	return "=?UTF-8?B?" + encoded + "?="
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
		if line == "" {
			continue
		}
		log.Printf("C: %s", line)
		s.handleCommand(line)
	}
}

func (s *IMAPSession) handleCommand(line string) {
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
		s.send("* CAPABILITY IMAP4rev1 AUTH=PLAIN IDLE")
		s.send("%s OK CAPABILITY completed", tag)
	case "NOOP":
		if s.store.CheckReload() && s.state == StateSelected {
			msgs := s.store.MessagesInFolder(s.selectedFolder)
			s.send("* %d EXISTS", len(msgs))
		}
		s.send("%s OK NOOP completed", tag)
	case "LOGOUT":
		s.send("* BYE OrgMail server logging out")
		s.send("%s OK LOGOUT completed", tag)
		s.state = StateLogout
	case "LOGIN":
		s.state = StateAuthenticated
		s.send("%s OK LOGIN completed", tag)
	case "AUTHENTICATE":
		if strings.ToUpper(args) == "PLAIN" {
			s.send("+")
			s.reader.ReadString('\n')
			s.state = StateAuthenticated
			s.send("%s OK AUTHENTICATE completed", tag)
		} else {
			s.send("%s NO Unsupported mechanism", tag)
		}
	case "LIST":
		s.handleList(tag, args)
	case "LSUB":
		s.handleList(tag, args)
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
	case "CHECK":
		s.store.CheckReload()
		s.send("%s OK CHECK completed", tag)
	case "CREATE", "DELETE", "RENAME", "SUBSCRIBE", "UNSUBSCRIBE":
		s.send("%s OK %s completed", tag, cmd)
	case "COPY":
		s.send("%s NO COPY not supported", tag)
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
	ref, pattern := parseListArgs(args)
	_ = ref

	if pattern == "" {
		s.send(`* LIST (\Noselect) "/" ""`)
		s.send("%s OK LIST completed", tag)
		return
	}

	folders := s.store.FolderPaths()

	regexPattern := "^" + strings.ReplaceAll(strings.ReplaceAll(
		regexp.QuoteMeta(pattern), `\*`, ".*"), `\%`, "[^/]*") + "$"
	re, err := regexp.Compile(regexPattern)
	if err != nil {
		s.send("%s BAD Invalid pattern", tag)
		return
	}

	for _, f := range folders {
		if re.MatchString(f) {
			hasChildren := false
			for _, f2 := range folders {
				if strings.HasPrefix(f2, f+"/") {
					hasChildren = true
					break
				}
			}

			// Determine attributes
			var attrs []string
			if hasChildren {
				attrs = append(attrs, `\HasChildren`)
			} else {
				attrs = append(attrs, `\HasNoChildren`)
			}

			// Year and Month folders have no messages — mark as \Noselect
			depth := strings.Count(f, "/")
			if f != "INBOX" && depth < 2 {
				attrs = append(attrs, `\Noselect`)
			}

			s.send(`* LIST (%s) "/" "%s"`, strings.Join(attrs, " "), f)
		}
	}
	s.send("%s OK LIST completed", tag)
}

func parseListArgs(args string) (string, string) {
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
		if s[0] == '"' {
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

	// Verify the folder exists
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

	msgs := s.store.MessagesInFolder(folder)
	if msgs == nil {
		msgs = []*OrgMessage{}
	}

	s.selectedFolder = folder
	s.readonly = readonly
	s.state = StateSelected

	unseen := 0
	for _, m := range msgs {
		if !containsFlag(m.Flags, "\\Seen") {
			unseen++
		}
	}

	s.send("* %d EXISTS", len(msgs))
	s.send("* 0 RECENT")
	s.send("* OK [UIDVALIDITY %d] UIDs valid", s.store.UIDValidity())

	nextUID := uint32(1)
	if len(msgs) > 0 {
		nextUID = msgs[len(msgs)-1].UID + 1
	}
	s.send("* OK [UIDNEXT %d] Predicted next UID", nextUID)
	s.send("* FLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft)")
	s.send("* OK [PERMANENTFLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft \\*)]")

	if unseen > 0 {
		s.send("* OK [UNSEEN 1]")
	}

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
	parts := parseQuotedStrings(args)
	if len(parts) < 1 {
		s.send("%s BAD Invalid STATUS arguments", tag)
		return
	}
	folder := parts[0]
	msgs := s.store.MessagesInFolder(folder)
	if msgs == nil {
		msgs = []*OrgMessage{}
	}
	count := len(msgs)

	unseen := 0
	for _, m := range msgs {
		if !containsFlag(m.Flags, "\\Seen") {
			unseen++
		}
	}

	nextUID := uint32(1)
	if count > 0 {
		nextUID = msgs[count-1].UID + 1
	}

	s.send(`* STATUS "%s" (MESSAGES %d RECENT 0 UIDNEXT %d UIDVALIDITY %d UNSEEN %d)`,
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
	if msgs == nil {
		msgs = []*OrgMessage{}
	}

	seqPart, dataPart := splitFetchArgs(args)
	seqNums := parseSequenceSet(seqPart, len(msgs))

	for _, seq := range seqNums {
		if seq < 1 || seq > len(msgs) {
			continue
		}
		msg := msgs[seq-1]
		response := s.buildFetchResponse(msg, dataPart, false)
		s.send("* %d FETCH (%s)", seq, response)
	}

	s.send("%s OK FETCH completed", tag)
}

// ---------------------------------------------------------------------------
// UID commands
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
		s.send("%s NO COPY not supported", tag)
	default:
		s.send("%s BAD Unknown UID subcommand", tag)
	}
}

func (s *IMAPSession) handleUIDFetch(tag, args string) {
	msgs := s.store.MessagesInFolder(s.selectedFolder)
	if msgs == nil {
		msgs = []*OrgMessage{}
	}

	seqPart, dataPart := splitFetchArgs(args)
	uids := parseUIDSet(seqPart, msgs)

	for _, uid := range uids {
		for seq, msg := range msgs {
			if msg.UID == uid {
				response := s.buildFetchResponse(msg, dataPart, true)
				s.send("* %d FETCH (%s)", seq+1, response)
				break
			}
		}
	}

	s.send("%s OK UID FETCH completed", tag)
}

func (s *IMAPSession) handleUIDSearch(tag, args string) {
	msgs := s.store.MessagesInFolder(s.selectedFolder)
	if msgs == nil {
		msgs = []*OrgMessage{}
	}

	criteria := strings.TrimSpace(args)

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
	if msgs == nil {
		msgs = []*OrgMessage{}
	}

	parts := strings.SplitN(args, " ", 3)
	if len(parts) < 3 {
		s.send("%s BAD Invalid UID STORE arguments", tag)
		return
	}

	uids := parseUIDSet(parts[0], msgs)
	action := strings.ToUpper(parts[1])
	newFlags := parseFlagList(parts[2])

	for _, uid := range uids {
		for seq, msg := range msgs {
			if msg.UID == uid {
				applyFlagAction(msg, action, newFlags)
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
	if msgs == nil {
		msgs = []*OrgMessage{}
	}

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
		applyFlagAction(msg, action, newFlags)
		if !strings.HasSuffix(action, ".SILENT") {
			s.send("* %d FETCH (FLAGS (%s))", seq, strings.Join(msg.Flags, " "))
		}
		s.store.UpdateFlags(msg.UID, msg.Flags)
	}

	s.send("%s OK STORE completed", tag)
}

func applyFlagAction(msg *OrgMessage, action string, newFlags []string) {
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
	default: // FLAGS or FLAGS.SILENT
		msg.Flags = newFlags
	}
	msg.IsTODO = containsFlag(msg.Flags, "\\Flagged")
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
	if msgs == nil {
		msgs = []*OrgMessage{}
	}
	criteria := strings.TrimSpace(args)

	var matched []string
	for i, msg := range msgs {
		if matchesCriteria(msg, criteria) {
			matched = append(matched, fmt.Sprintf("%d", i+1))
		}
	}

	if len(matched) > 0 {
		s.send("* SEARCH %s", strings.Join(matched, " "))
	} else {
		s.send("* SEARCH")
	}
	s.send("%s OK SEARCH completed", tag)
}

func matchesCriteria(msg *OrgMessage, criteria string) bool {
	upper := strings.ToUpper(criteria)
	if upper == "ALL" || upper == "" {
		return true
	}
	if strings.Contains(upper, "UNSEEN") {
		return !containsFlag(msg.Flags, "\\Seen")
	}
	if strings.Contains(upper, "SEEN") && !strings.Contains(upper, "UNSEEN") {
		return containsFlag(msg.Flags, "\\Seen")
	}
	if strings.Contains(upper, "FLAGGED") {
		return containsFlag(msg.Flags, "\\Flagged")
	}
	if strings.Contains(upper, "DELETED") {
		return containsFlag(msg.Flags, "\\Deleted")
	}
	if strings.Contains(upper, "NOT DELETED") {
		return !containsFlag(msg.Flags, "\\Deleted")
	}
	return true
}

// ---------------------------------------------------------------------------
// APPEND
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleAppend(tag, args string) {
	// APPEND "folder" (\flags) "date" {size}
	// or APPEND "folder" {size}
	folder, rest := parseFirstQuotedOrAtom(args)

	// Parse optional flags
	var flags []string
	if idx := strings.Index(rest, "("); idx != -1 {
		end := strings.Index(rest, ")")
		if end != -1 {
			flags = parseFlagList(rest[idx : end+1])
			rest = rest[end+1:]
		}
	}

	// Parse optional date-time (quoted)
	rest = strings.TrimSpace(rest)
	if len(rest) > 0 && rest[0] == '"' {
		endQ := strings.Index(rest[1:], "\"")
		if endQ != -1 {
			// dateStr := rest[1 : endQ+1]
			// We ignore the date from APPEND; we'll parse from headers
			rest = rest[endQ+2:]
		}
	}

	// Find literal size
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "{") || !strings.HasSuffix(rest, "}") {
		s.send("%s BAD Missing literal size", tag)
		return
	}
	sizeStr := rest[1 : len(rest)-1]
	// Handle optional + for literal+
	sizeStr = strings.TrimSuffix(sizeStr, "+")
	size, err := strconv.Atoi(sizeStr)
	if err != nil {
		s.send("%s BAD Invalid literal size", tag)
		return
	}

	s.send("+ Ready for literal data")

	buf := make([]byte, size)
	_, err = io.ReadFull(s.reader, buf)
	if err != nil {
		s.send("%s BAD Failed to read literal", tag)
		return
	}

	// Read trailing CRLF
	s.reader.ReadString('\n')

	subject, body, date := parseRFC2822Full(string(buf))

	_, err = s.store.AppendMessage(folder, date, subject, body, flags)
	if err != nil {
		s.send("%s NO APPEND failed: %v", tag, err)
		return
	}

	s.send("%s OK APPEND completed", tag)
}

func parseFirstQuotedOrAtom(s string) (string, string) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return "", ""
	}
	if s[0] == '"' {
		end := strings.Index(s[1:], "\"")
		if end == -1 {
			return s[1:], ""
		}
		return s[1 : end+1], strings.TrimSpace(s[end+2:])
	}
	idx := strings.IndexByte(s, ' ')
	if idx == -1 {
		return s, ""
	}
	return s[:idx], strings.TrimSpace(s[idx+1:])
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

	// Collect UIDs to delete (in reverse order to keep sequence numbers valid)
	type delEntry struct {
		seq int
		uid uint32
	}
	var toDelete []delEntry
	for i, msg := range msgs {
		if containsFlag(msg.Flags, "\\Deleted") {
			toDelete = append(toDelete, delEntry{seq: i + 1, uid: msg.UID})
		}
	}

	// Send EXPUNGE responses (must account for shifting)
	offset := 0
	for _, d := range toDelete {
		s.send("* %d EXPUNGE", d.seq-offset)
		offset++
	}

	for _, d := range toDelete {
		s.store.DeleteMessage(d.uid)
	}

	s.send("%s OK EXPUNGE completed", tag)
}

// ---------------------------------------------------------------------------
// IDLE
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleIdle(tag string) {
	s.send("+ idling")

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
	idx := strings.IndexByte(args, ' ')
	if idx == -1 {
		return args, "ALL"
	}
	return args[:idx], args[idx+1:]
}

func (s *IMAPSession) buildFetchResponse(msg *OrgMessage, dataItems string, includeUID bool) string {
	var parts []string

	items := strings.ToUpper(dataItems)

	// Expand macros
	switch strings.Trim(items, "()") {
	case "ALL":
		items = "FLAGS INTERNALDATE RFC822.SIZE ENVELOPE"
	case "FULL":
		items = "FLAGS INTERNALDATE RFC822.SIZE ENVELOPE BODY"
	case "FAST":
		items = "FLAGS INTERNALDATE RFC822.SIZE"
	}

	items = strings.Trim(items, "()")
	rfc2822 := formatRFC2822(msg)

	if includeUID || strings.Contains(items, "UID") {
		parts = append(parts, fmt.Sprintf("UID %d", msg.UID))
	}

	if strings.Contains(items, "FLAGS") {
		parts = append(parts, fmt.Sprintf("FLAGS (%s)", strings.Join(msg.Flags, " ")))
	}

	if strings.Contains(items, "INTERNALDATE") {
		parts = append(parts, fmt.Sprintf(`INTERNALDATE "%s"`,
			msg.Date.Format("02-Jan-2006 15:04:05 -0700")))
	}

	if strings.Contains(items, "RFC822.SIZE") {
		parts = append(parts, fmt.Sprintf("RFC822.SIZE %d", len(rfc2822)))
	}

	if strings.Contains(items, "RFC822.HEADER") {
		header := extractRFC2822Header(rfc2822)
		parts = append(parts, fmt.Sprintf("RFC822.HEADER {%d}\r\n%s", len(header), header))
	}

	// BODY.PEEK[HEADER.FIELDS (...)] or BODY[HEADER.FIELDS (...)]
	if idx := strings.Index(items, "HEADER.FIELDS"); idx != -1 {
		header := extractRFC2822Header(rfc2822)
		fieldStart := strings.Index(items[idx:], "(")
		fieldEnd := strings.Index(items[idx:], ")")
		requestedFields := ""
		if fieldStart != -1 && fieldEnd != -1 {
			requestedFields = items[idx+fieldStart+1 : idx+fieldEnd]
		}
		filtered := filterHeaders(header, requestedFields)
		itemName := "BODY[HEADER.FIELDS (" + requestedFields + ")]"
		parts = append(parts, fmt.Sprintf("%s {%d}\r\n%s", itemName, len(filtered), filtered))
	} else if strings.Contains(items, "BODY.PEEK[]") || strings.Contains(items, "BODY[]") {
		parts = append(parts, fmt.Sprintf("BODY[] {%d}\r\n%s", len(rfc2822), rfc2822))
	} else if strings.Contains(items, "BODY.PEEK[HEADER]") || strings.Contains(items, "BODY[HEADER]") {
		header := extractRFC2822Header(rfc2822)
		parts = append(parts, fmt.Sprintf("BODY[HEADER] {%d}\r\n%s", len(header), header))
	} else if strings.Contains(items, "BODY.PEEK[TEXT]") || strings.Contains(items, "BODY[TEXT]") {
		text := extractRFC2822Body(rfc2822)
		parts = append(parts, fmt.Sprintf("BODY[TEXT] {%d}\r\n%s", len(text), text))
	} else if strings.Contains(items, "RFC822") && !strings.Contains(items, "RFC822.SIZE") && !strings.Contains(items, "RFC822.HEADER") {
		parts = append(parts, fmt.Sprintf("RFC822 {%d}\r\n%s", len(rfc2822), rfc2822))
	}

	if strings.Contains(items, "ENVELOPE") {
		env := buildEnvelope(msg)
		parts = append(parts, "ENVELOPE "+env)
	}

	if strings.Contains(items, "BODYSTRUCTURE") {
		bodyLen := len(msg.Body)
		bodyLines := strings.Count(msg.Body, "\n") + 1
		parts = append(parts, fmt.Sprintf(
			`BODYSTRUCTURE ("TEXT" "PLAIN" ("CHARSET" "UTF-8") NIL NIL "8BIT" %d %d)`,
			bodyLen, bodyLines))
	}

	return strings.Join(parts, " ")
}

func extractRFC2822Header(rfc2822 string) string {
	idx := strings.Index(rfc2822, "\r\n\r\n")
	if idx == -1 {
		return rfc2822
	}
	return rfc2822[:idx+4]
}

func extractRFC2822Body(rfc2822 string) string {
	idx := strings.Index(rfc2822, "\r\n\r\n")
	if idx == -1 {
		return ""
	}
	return rfc2822[idx+4:]
}

func buildEnvelope(msg *OrgMessage) string {
	return fmt.Sprintf(`("%s" "%s" (("orgmail" NIL "orgmail" "localhost")) (("orgmail" NIL "orgmail" "localhost")) (("orgmail" NIL "orgmail" "localhost")) (("user" NIL "user" "localhost")) NIL NIL NIL "<org-%d@localhost>")`,
		msg.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700"),
		escapeIMAPString(msg.Subject),
		msg.UID)
}

func filterHeaders(header string, fields string) string {
	wanted := make(map[string]bool)
	for _, f := range strings.Fields(fields) {
		wanted[strings.ToUpper(strings.TrimSpace(f))] = true
	}

	var result bytes.Buffer
	lines := strings.Split(header, "\r\n")
	include := false
	for _, line := range lines {
		if line == "" {
			result.WriteString("\r\n")
			continue
		}
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			colonIdx := strings.IndexByte(line, ':')
			if colonIdx > 0 {
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
	for _, part := range strings.Split(strings.TrimSpace(set), ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, ":") {
			rp := strings.SplitN(part, ":", 2)
			start := resolveSeqNum(rp[0], total)
			end := resolveSeqNum(rp[1], total)
			if start > end {
				start, end = end, start
			}
			for i := start; i <= end; i++ {
				if i >= 1 && i <= total {
					result = append(result, i)
				}
			}
		} else {
			n := resolveSeqNum(part, total)
			if n >= 1 && n <= total {
				result = append(result, n)
			}
		}
	}
	return result
}

func resolveSeqNum(s string, total int) int {
	if strings.TrimSpace(s) == "*" {
		return total
	}
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func parseUIDSet(set string, msgs []*OrgMessage) []uint32 {
	var result []uint32
	if len(msgs) == 0 {
		return result
	}
	maxUID := msgs[len(msgs)-1].UID

	for _, part := range strings.Split(strings.TrimSpace(set), ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, ":") {
			rp := strings.SplitN(part, ":", 2)
			start := resolveUID(rp[0], maxUID)
			end := resolveUID(rp[1], maxUID)
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
	if strings.TrimSpace(s) == "*" {
		return maxUID
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	return uint32(n)
}

// ---------------------------------------------------------------------------
// Flag helpers
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
		if f != "" {
			flags = append(flags, f)
		}
	}
	return flags
}

func containsFlag(flags []string, flag string) bool {
	upper := strings.ToUpper(flag)
	for _, f := range flags {
		if strings.ToUpper(f) == upper {
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

	log.Printf("Loaded org file: %s (timezone: %s)", orgFile, store.localLoc.String())
	folders := store.FolderPaths()
	for _, f := range folders {
		msgs := store.MessagesInFolder(f)
		if len(msgs) > 0 {
			log.Printf("  [%s] %d messages", f, len(msgs))
		}
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}
	defer listener.Close()

	log.Printf("IMAP server listening on %s", listenAddr)

	// Background file watcher
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if store.CheckReload() {
				log.Println("Org file changed on disk, reloaded")
			}
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
		log.Printf("Connection from %s", conn.RemoteAddr())
		go NewIMAPSession(conn, store).Run()
	}
}