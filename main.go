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

type OrgNode struct {
	Title    string
	BodyText string     // prose directly under this heading (before any child)
	Children []*OrgNode // sub-headings
	Level    int        // 1 for *, 2 for **, etc.
}

type DraftMessage struct {
	UID     uint32
	Date    time.Time
	Subject string
	Body    string
	Flags   []string
}

type VirtualMessage struct {
	UID     uint32
	Date    time.Time
	Subject string
	Body    string
	Flags   []string
	Node    *OrgNode
}

type OrgStore struct {
	mu          sync.RWMutex
	filePath    string
	roots       []*OrgNode
	drafts      []*DraftMessage
	nextUID     uint32
	modTime     time.Time
	uidValidity uint32
	lastHash    [16]byte
	localLoc    *time.Location

	vmCache    map[string]*VirtualMessage
	uidToVM    map[uint32]*VirtualMessage
	uidToDraft map[uint32]*DraftMessage
}

func NewOrgStore(path string) *OrgStore {
	return &OrgStore{
		filePath:    path,
		nextUID:     1,
		uidValidity: uint32(time.Now().Unix()),
		localLoc:    time.Now().Location(),
		vmCache:     make(map[string]*VirtualMessage),
		uidToVM:     make(map[uint32]*VirtualMessage),
		uidToDraft:  make(map[uint32]*DraftMessage),
	}
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

func (s *OrgStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *OrgStore) loadLocked() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			s.roots = nil
			s.drafts = nil
			s.rebuildCache()
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

	s.roots = nil
	s.drafts = nil
	s.nextUID = 1

	lines := strings.Split(string(data), "\n")
	s.parseLines(lines)
	s.rebuildCache()
	return nil
}

var reHeading = regexp.MustCompile(`^(\*+)\s+(.+)$`)

func (s *OrgStore) parseLines(lines []string) {
	type stackEntry struct {
		node  *OrgNode
		level int
	}

	var stack []stackEntry
	var currentNode *OrgNode // node whose BodyText we're currently appending to
	inDrafts := false
	var draftMsg *DraftMessage
	var draftBodyLines []string

	flushDraft := func() {
		if draftMsg != nil {
			draftMsg.Body = trimBody(strings.Join(draftBodyLines, "\n"))
			s.drafts = append(s.drafts, draftMsg)
			draftMsg = nil
			draftBodyLines = nil
		}
	}

	for _, line := range lines {
		m := reHeading.FindStringSubmatch(line)
		if m != nil {
			stars := len(m[1])
			title := strings.TrimSpace(m[2])

			// Check if this is "Drafts" at level 1
			if stars == 1 && strings.EqualFold(title, "Drafts") {
				flushDraft()
				inDrafts = true
				currentNode = nil
				stack = nil
				continue
			}

			if inDrafts {
				if stars == 1 {
					// Exiting Drafts — new top-level heading
					flushDraft()
					inDrafts = false
					// Fall through to normal handling below
				} else if stars == 2 {
					flushDraft()
					date, subj, flags := parseDraftHeading(title, s.localLoc)
					draftMsg = &DraftMessage{
						UID:     s.nextUID,
						Date:    date,
						Subject: subj,
						Flags:   flags,
					}
					s.nextUID++
					draftBodyLines = nil
					continue
				} else {
					// Deeper heading inside a draft: treat as body
					if draftMsg != nil {
						draftBodyLines = append(draftBodyLines, line)
					}
					continue
				}
			}

			if inDrafts {
				continue
			}

			// Normal heading
			node := &OrgNode{
				Title: title,
				Level: stars,
			}

			// Pop stack until we find the parent
			for len(stack) > 0 && stack[len(stack)-1].level >= stars {
				stack = stack[:len(stack)-1]
			}

			if len(stack) == 0 {
				s.roots = append(s.roots, node)
			} else {
				parent := stack[len(stack)-1].node
				parent.Children = append(parent.Children, node)
			}

			stack = append(stack, stackEntry{node: node, level: stars})
			currentNode = node
			continue
		}

		// Body line
		if inDrafts && draftMsg != nil {
			draftBodyLines = append(draftBodyLines, line)
			continue
		}

		if currentNode != nil {
			if currentNode.BodyText == "" {
				currentNode.BodyText = line
			} else {
				currentNode.BodyText = currentNode.BodyText + "\n" + line
			}
		}
	}

	flushDraft()
	s.trimAllBodies(s.roots)
}

func (s *OrgStore) trimAllBodies(nodes []*OrgNode) {
	for _, n := range nodes {
		n.BodyText = trimBody(n.BodyText)
		s.trimAllBodies(n.Children)
	}
}

func trimBody(body string) string {
	body = trimLeadingEmptyLines(body)
	body = strings.TrimRight(body, "\n\r \t")
	return body
}

func parseDraftHeading(title string, loc *time.Location) (time.Time, string, []string) {
	re := regexp.MustCompile(`^(TODO\s+)?\[(\d{4}-\d{2}-\d{2}\s+\w+\s+\d{1,2}:\d{2})\]\s*(.*)$`)
	m := re.FindStringSubmatch(title)
	if m != nil {
		var flags []string
		if strings.TrimSpace(m[1]) == "TODO" {
			flags = append(flags, "\\Flagged")
		}
		t, err := time.ParseInLocation("2006-01-02 Mon 15:04", m[2], loc)
		if err != nil {
			t = time.Now().In(loc)
		}
		return t, m[3], flags
	}
	return time.Now().In(loc), title, nil
}

// ---------------------------------------------------------------------------
// Cache rebuild
// ---------------------------------------------------------------------------

func (s *OrgStore) rebuildCache() {
	s.vmCache = make(map[string]*VirtualMessage)
	s.uidToVM = make(map[uint32]*VirtualMessage)
	s.uidToDraft = make(map[uint32]*DraftMessage)

	for _, root := range s.roots {
		folderPath := "INBOX/" + root.Title
		s.buildVMForNode(root, folderPath)
	}

	for _, d := range s.drafts {
		s.uidToDraft[d.UID] = d
	}
}

func (s *OrgStore) buildVMForNode(node *OrgNode, folderPath string) {
	if node.BodyText != "" {
		vm := &VirtualMessage{
			UID:     s.nextUID,
			Date:    s.extractDateFromBody(node.BodyText),
			Subject: node.Title,
			Body:    node.BodyText,
			Node:    node,
		}
		s.nextUID++
		s.vmCache[folderPath] = vm
		s.uidToVM[vm.UID] = vm
	}

	for _, child := range node.Children {
		childPath := folderPath + "/" + child.Title
		s.buildVMForNode(child, childPath)
	}
}

func (s *OrgStore) extractDateFromBody(body string) time.Time {
	re := regexp.MustCompile(`\[(\d{4}-\d{2}-\d{2}\s+\w+\s+\d{1,2}:\d{2})\]`)
	m := re.FindStringSubmatch(body)
	if m != nil {
		t, err := time.ParseInLocation("2006-01-02 Mon 15:04", m[1], s.localLoc)
		if err == nil {
			return t
		}
	}
	return time.Now().In(s.localLoc)
}

// ---------------------------------------------------------------------------
// Saving
// ---------------------------------------------------------------------------

func (s *OrgStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *OrgStore) saveLocked() error {
	var sb strings.Builder

	for _, root := range s.roots {
		s.writeNode(&sb, root, 1)
	}

	if len(s.drafts) > 0 {
		sb.WriteString("* Drafts\n")
		for _, d := range s.drafts {
			sb.WriteString("** ")
			if containsFlag(d.Flags, "\\Flagged") {
				sb.WriteString("TODO ")
			}
			fmt.Fprintf(&sb, "[%s] %s\n",
				d.Date.In(s.localLoc).Format("2006-01-02 Mon 15:04"),
				d.Subject)
			if d.Body != "" {
				sb.WriteString(d.Body)
				sb.WriteString("\n")
			}
		}
	}

	data := []byte(sb.String())
	s.lastHash = md5.Sum(data)
	return os.WriteFile(s.filePath, data, 0644)
}

func (s *OrgStore) writeNode(sb *strings.Builder, node *OrgNode, level int) {
	stars := strings.Repeat("*", level)
	fmt.Fprintf(sb, "%s %s\n", stars, node.Title)
	if node.BodyText != "" {
		sb.WriteString(node.BodyText)
		sb.WriteString("\n")
	}
	for _, child := range node.Children {
		s.writeNode(sb, child, level+1)
	}
}

// ---------------------------------------------------------------------------
// Folder operations
// ---------------------------------------------------------------------------

func (s *OrgStore) FolderPaths() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var paths []string
	paths = append(paths, "INBOX")
	for _, root := range s.roots {
		s.collectPaths(&paths, root, "INBOX/"+root.Title)
	}
	paths = append(paths, "Drafts")
	return paths
}

func (s *OrgStore) collectPaths(paths *[]string, node *OrgNode, prefix string) {
	*paths = append(*paths, prefix)
	for _, child := range node.Children {
		s.collectPaths(paths, child, prefix+"/"+child.Title)
	}
}

func (s *OrgStore) MessagesInFolder(folder string) []*VirtualMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if strings.EqualFold(folder, "INBOX") {
		return nil
	}

	if strings.EqualFold(folder, "Drafts") {
		var vms []*VirtualMessage
		for _, d := range s.drafts {
			vms = append(vms, &VirtualMessage{
				UID:     d.UID,
				Date:    d.Date,
				Subject: d.Subject,
				Body:    d.Body,
				Flags:   d.Flags,
			})
		}
		return vms
	}

	vm, ok := s.vmCache[folder]
	if ok {
		return []*VirtualMessage{vm}
	}

	return nil
}

func (s *OrgStore) findNode(folder string) *OrgNode {
	if !strings.HasPrefix(folder, "INBOX/") {
		return nil
	}
	rest := folder[len("INBOX/"):]
	parts := strings.Split(rest, "/")
	if len(parts) == 0 {
		return nil
	}

	var cur *OrgNode
	for _, r := range s.roots {
		if r.Title == parts[0] {
			cur = r
			break
		}
	}
	if cur == nil {
		return nil
	}

	for _, p := range parts[1:] {
		found := false
		for _, c := range cur.Children {
			if c.Title == p {
				cur = c
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return cur
}

func (s *OrgStore) findOrCreateNode(folder string) *OrgNode {
	if !strings.HasPrefix(folder, "INBOX/") {
		return nil
	}
	rest := folder[len("INBOX/"):]
	parts := strings.Split(rest, "/")
	if len(parts) == 0 {
		return nil
	}

	var cur *OrgNode
	for _, r := range s.roots {
		if r.Title == parts[0] {
			cur = r
			break
		}
	}
	if cur == nil {
		cur = &OrgNode{Title: parts[0], Level: 1}
		s.roots = append(s.roots, cur)
	}

	for i, p := range parts[1:] {
		found := false
		for _, c := range cur.Children {
			if c.Title == p {
				cur = c
				found = true
				break
			}
		}
		if !found {
			child := &OrgNode{Title: p, Level: i + 2}
			cur.Children = append(cur.Children, child)
			cur = child
		}
	}
	return cur
}

func (s *OrgStore) AppendMessage(folder string, date time.Time, subject, body string, flags []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	date = date.In(s.localLoc)

	if !utf8.ValidString(body) {
		body = strings.ToValidUTF8(body, "?")
	}
	if !utf8.ValidString(subject) {
		subject = strings.ToValidUTF8(subject, "?")
	}
	body = trimBody(body)

	// Drafts
	if strings.EqualFold(folder, "Drafts") || strings.HasPrefix(strings.ToLower(folder), "drafts/") {
		dm := &DraftMessage{
			UID:     s.nextUID,
			Date:    date,
			Subject: subject,
			Body:    body,
			Flags:   flags,
		}
		s.nextUID++
		s.drafts = append(s.drafts, dm)
		s.uidToDraft[dm.UID] = dm
		return s.saveLocked()
	}

	// For INBOX/* folders: merge into node body
	node := s.findOrCreateNode(folder)
	if node == nil {
		return fmt.Errorf("cannot resolve folder %s", folder)
	}

	if node.BodyText == "" {
		node.BodyText = body
	} else {
		node.BodyText = node.BodyText + "\n\n" + body
	}

	s.rebuildCache()
	return s.saveLocked()
}

func (s *OrgStore) DeleteMessage(uid uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, d := range s.drafts {
		if d.UID == uid {
			s.drafts = append(s.drafts[:i], s.drafts[i+1:]...)
			delete(s.uidToDraft, uid)
			return s.saveLocked()
		}
	}

	if vm, ok := s.uidToVM[uid]; ok && vm.Node != nil {
		vm.Node.BodyText = ""
		delete(s.uidToVM, uid)
		for k, v := range s.vmCache {
			if v.UID == uid {
				delete(s.vmCache, k)
				break
			}
		}
		return s.saveLocked()
	}

	return fmt.Errorf("UID %d not found", uid)
}

func (s *OrgStore) UpdateFlags(uid uint32, flags []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if vm, ok := s.uidToVM[uid]; ok {
		vm.Flags = flags
		return nil
	}
	if dm, ok := s.uidToDraft[uid]; ok {
		dm.Flags = flags
		return s.saveLocked()
	}
	return fmt.Errorf("UID %d not found", uid)
}

func (s *OrgStore) CheckReload() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := os.Stat(s.filePath)
	if err != nil {
		return false
	}
	if info.ModTime().After(s.modTime) {
		if err := s.loadLocked(); err != nil {
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
// HTML to Markdown
// ---------------------------------------------------------------------------

func htmlToMarkdown(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}

	r := s
	r = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`).ReplaceAllString(r, "")
	r = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).ReplaceAllString(r, "")
	r = regexp.MustCompile(`(?i)<br\s*/?\s*>`).ReplaceAllString(r, "\n")
	r = regexp.MustCompile(`(?i)<p[^>]*>`).ReplaceAllString(r, "\n\n")
	r = regexp.MustCompile(`(?i)</p>`).ReplaceAllString(r, "\n")
	r = regexp.MustCompile(`(?i)<div[^>]*>`).ReplaceAllString(r, "\n")
	r = regexp.MustCompile(`(?i)</div>`).ReplaceAllString(r, "\n")
	r = regexp.MustCompile(`(?i)<hr\s*/?\s*>`).ReplaceAllString(r, "\n---\n")

	for i := 6; i >= 1; i-- {
		prefix := strings.Repeat("#", i)
		re := regexp.MustCompile(fmt.Sprintf(`(?is)<h%d[^>]*>(.*?)</h%d>`, i, i))
		r = re.ReplaceAllString(r, "\n"+prefix+" $1\n")
	}

	r = regexp.MustCompile(`(?is)<(?:b|strong)[^>]*>(.*?)</(?:b|strong)>`).ReplaceAllString(r, "**$1**")
	r = regexp.MustCompile(`(?is)<(?:i|em)[^>]*>(.*?)</(?:i|em)>`).ReplaceAllString(r, "*$1*")
	r = regexp.MustCompile(`(?is)<code[^>]*>(.*?)</code>`).ReplaceAllString(r, "~$1~")
	r = regexp.MustCompile(`(?is)<pre[^>]*>(.*?)</pre>`).ReplaceAllString(r, "\n#+BEGIN_EXAMPLE\n$1\n#+END_EXAMPLE\n")
	r = regexp.MustCompile(`(?is)<a\s+[^>]*href\s*=\s*"([^"]*)"[^>]*>(.*?)</a>`).ReplaceAllString(r, "[[$1][$2]]")
	r = regexp.MustCompile(`(?i)<img\s+[^>]*src\s*=\s*"([^"]*)"[^>]*/?\s*>`).ReplaceAllString(r, "[[$1]]")
	r = regexp.MustCompile(`(?i)<li[^>]*>`).ReplaceAllString(r, "\n- ")
	r = regexp.MustCompile(`(?i)</li>`).ReplaceAllString(r, "")
	r = regexp.MustCompile(`(?i)</?(?:ul|ol)[^>]*>`).ReplaceAllString(r, "\n")
	r = regexp.MustCompile(`(?i)<blockquote[^>]*>`).ReplaceAllString(r, "\n#+BEGIN_QUOTE\n")
	r = regexp.MustCompile(`(?i)</blockquote>`).ReplaceAllString(r, "\n#+END_QUOTE\n")
	r = regexp.MustCompile(`(?i)<tr[^>]*>`).ReplaceAllString(r, "|")
	r = regexp.MustCompile(`(?i)</tr>`).ReplaceAllString(r, "|\n")
	r = regexp.MustCompile(`(?i)<t[dh][^>]*>`).ReplaceAllString(r, " ")
	r = regexp.MustCompile(`(?i)</t[dh]>`).ReplaceAllString(r, " |")
	r = regexp.MustCompile(`(?i)</?table[^>]*>`).ReplaceAllString(r, "\n")
	r = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(r, "")
	r = decodeHTMLEntities(r)
	r = regexp.MustCompile(`\n{3,}`).ReplaceAllString(r, "\n\n")
	r = trimLeadingEmptyLines(r)
	r = strings.TrimRight(r, "\n\r \t")

	return r
}

func decodeHTMLEntities(s string) string {
	entities := map[string]string{
		"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": "\"",
		"&apos;": "'", "&#39;": "'", "&nbsp;": " ", "&mdash;": "—",
		"&ndash;": "–", "&laquo;": "«", "&raquo;": "»", "&copy;": "©",
		"&reg;": "®", "&trade;": "™", "&hellip;": "…",
	}
	r := s
	for e, c := range entities {
		r = strings.ReplaceAll(r, e, c)
	}
	r = regexp.MustCompile(`&#(\d+);`).ReplaceAllStringFunc(r, func(match string) string {
		n, err := strconv.Atoi(match[2 : len(match)-1])
		if err != nil || n < 0 || n > 0x10FFFF {
			return match
		}
		return string(rune(n))
	})
	r = regexp.MustCompile(`(?i)&#x([0-9a-f]+);`).ReplaceAllStringFunc(r, func(match string) string {
		n, err := strconv.ParseInt(match[3:len(match)-1], 16, 32)
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

func decodeMIMESubject(s string) string {
	dec := new(mime.WordDecoder)
	result, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return result
}

func parseRFC2822Full(raw string, loc *time.Location) (subject, body string, date time.Time) {
	date = time.Now().In(loc)

	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	headerBody := strings.SplitN(normalized, "\n\n", 2)

	headerSection := ""
	rawBody := ""
	if len(headerBody) >= 1 {
		headerSection = headerBody[0]
	}
	if len(headerBody) >= 2 {
		rawBody = headerBody[1]
	}

	headers := parseHeaders(headerSection)

	if v, ok := headers["subject"]; ok {
		subject = decodeMIMESubject(v)
	}

	if v, ok := headers["date"]; ok {
		for _, layout := range []string{
			time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822,
			"Mon, 2 Jan 2006 15:04:05 -0700",
			"Mon, 02 Jan 2006 15:04:05 -0700",
			"2 Jan 2006 15:04:05 -0700",
			"Mon, 2 Jan 2006 15:04:05 -0700 (MST)",
		} {
			if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
				date = t.In(loc)
				break
			}
		}
	}

	contentType := headers["content-type"]
	transferEncoding := strings.ToLower(strings.TrimSpace(headers["content-transfer-encoding"]))

	body = extractBody(rawBody, contentType, transferEncoding)

	if !utf8.ValidString(body) {
		body = strings.ToValidUTF8(body, "?")
	}
	body = trimLeadingEmptyLines(body)
	body = strings.TrimRight(body, "\n\r \t")

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
		line = strings.TrimRight(line, "\r")
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
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
		contentType = "text/plain; charset=utf-8"
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return decodeTransferEncoding(rawBody, transferEncoding)
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary != "" {
			return parseMultipart(rawBody, boundary)
		}
		return decodeTransferEncoding(rawBody, transferEncoding)
	}

	decoded := decodeTransferEncoding(rawBody, transferEncoding)
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

	var textPlain, textHTML string

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

		mediaType, params, _ := mime.ParseMediaType(partCT)

		if strings.HasPrefix(mediaType, "multipart/") {
			if b := params["boundary"]; b != "" {
				if result := parseMultipart(string(partBytes), b); result != "" {
					return result
				}
			}
			continue
		}

		decoded := decodeTransferEncoding(string(partBytes), partTE)
		if charset, ok := params["charset"]; ok {
			decoded = ensureUTF8(decoded, charset)
		}

		if strings.HasPrefix(mediaType, "text/plain") {
			textPlain = decoded
		} else if strings.HasPrefix(mediaType, "text/html") {
			textHTML = decoded
		}
	}

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
		cleaned := strings.Join(strings.Fields(s), "")
		for len(cleaned)%4 != 0 {
			cleaned += "="
		}
		decoded, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return s
		}
		return string(decoded)
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(s)))
		if err != nil {
			return s
		}
		return string(decoded)
	default:
		return s
	}
}

func ensureUTF8(s, charset string) string {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "utf-8", "utf8", "us-ascii", "ascii", "":
		return s
	default:
		if utf8.ValidString(s) {
			return s
		}
		return strings.ToValidUTF8(s, "?")
	}
}

// ---------------------------------------------------------------------------
// RFC 2822 output formatting
// ---------------------------------------------------------------------------

func formatRFC2822(msg *VirtualMessage) string {
	var sb strings.Builder
	sb.WriteString("From: orgmail@localhost\r\n")
	sb.WriteString("To: user@localhost\r\n")
	fmt.Fprintf(&sb, "Date: %s\r\n", msg.Date.Format(time.RFC1123Z))
	fmt.Fprintf(&sb, "Subject: %s\r\n", encodeSubjectRFC2047(msg.Subject))
	fmt.Fprintf(&sb, "Message-ID: <org-%d@localhost>\r\n", msg.UID)
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
	for _, r := range s {
		if r > 127 {
			return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
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
	args := ""
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
		if !re.MatchString(f) {
			continue
		}

		var attrs []string

		hasChildren := false
		for _, f2 := range folders {
			if strings.HasPrefix(f2, f+"/") {
				hasChildren = true
				break
			}
		}

		if hasChildren {
			attrs = append(attrs, `\HasChildren`)
		} else {
			attrs = append(attrs, `\HasNoChildren`)
		}

		if f == "INBOX" {
			attrs = append(attrs, `\Noselect`)
		}

		s.send(`* LIST (%s) "/" "%s"`, strings.Join(attrs, " "), f)
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
		if len(s) == 0 {
			break
		}
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

	if strings.EqualFold(folder, "INBOX") {
		s.selectedFolder = folder
		s.readonly = readonly
		s.state = StateSelected
		s.send("* 0 EXISTS")
		s.send("* 0 RECENT")
		s.send("* OK [UIDVALIDITY %d]", s.store.UIDValidity())
		s.send("* OK [UIDNEXT 1]")
		s.send("* FLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft)")
		s.send("* OK [PERMANENTFLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft \\*)]")
		if readonly {
			s.send("%s OK [READ-ONLY] EXAMINE completed", tag)
		} else {
			s.send("%s OK [READ-WRITE] SELECT completed", tag)
		}
		return
	}

	allFolders := s.store.FolderPaths()
	found := false
	for _, f := range allFolders {
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
		msgs = []*VirtualMessage{}
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
	s.send("* OK [UIDNEXT %d]", nextUID)
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
		msgs = []*VirtualMessage{}
	}

	unseen := 0
	for _, m := range msgs {
		if !containsFlag(m.Flags, "\\Seen") {
			unseen++
		}
	}

	nextUID := uint32(1)
	if len(msgs) > 0 {
		nextUID = msgs[len(msgs)-1].UID + 1
	}

	s.send(`* STATUS "%s" (MESSAGES %d RECENT 0 UIDNEXT %d UIDVALIDITY %d UNSEEN %d)`,
		folder, len(msgs), nextUID, s.store.UIDValidity(), unseen)
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
		msgs = []*VirtualMessage{}
	}

	seqPart, dataPart := splitFetchArgs(args)
	seqNums := parseSequenceSet(seqPart, len(msgs))

	for _, seq := range seqNums {
		if seq < 1 || seq > len(msgs) {
			continue
		}
		msg := msgs[seq-1]
		response := buildFetchResponse(msg, dataPart, false)
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

	switch strings.ToUpper(parts[0]) {
	case "FETCH":
		s.handleUIDFetch(tag, parts[1])
	case "SEARCH":
		s.handleUIDSearch(tag, parts[1])
	case "STORE":
		s.handleUIDStore(tag, parts[1])
	case "COPY":
		s.send("%s NO COPY not supported", tag)
	default:
		s.send("%s BAD Unknown UID subcommand", tag)
	}
}

func (s *IMAPSession) handleUIDFetch(tag, args string) {
	msgs := s.store.MessagesInFolder(s.selectedFolder)
	if msgs == nil {
		msgs = []*VirtualMessage{}
	}

	seqPart, dataPart := splitFetchArgs(args)
	uids := parseUIDSet(seqPart, msgs)

	for _, uid := range uids {
		for seq, msg := range msgs {
			if msg.UID == uid {
				response := buildFetchResponse(msg, dataPart, true)
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
		msgs = []*VirtualMessage{}
	}

	criteria := strings.TrimSpace(args)
	var matched []string
	for _, msg := range msgs {
		if matchesCriteria(msg, criteria) {
			matched = append(matched, fmt.Sprintf("%d", msg.UID))
		}
	}

	if len(matched) > 0 {
		s.send("* SEARCH %s", strings.Join(matched, " "))
	} else {
		s.send("* SEARCH")
	}
	s.send("%s OK UID SEARCH completed", tag)
}

func (s *IMAPSession) handleUIDStore(tag, args string) {
	msgs := s.store.MessagesInFolder(s.selectedFolder)
	if msgs == nil {
		msgs = []*VirtualMessage{}
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
		msgs = []*VirtualMessage{}
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

func applyFlagAction(msg *VirtualMessage, action string, newFlags []string) {
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
	default:
		msg.Flags = newFlags
	}
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
		msgs = []*VirtualMessage{}
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

func matchesCriteria(msg *VirtualMessage, criteria string) bool {
	upper := strings.ToUpper(criteria)
	if upper == "ALL" || upper == "" {
		return true
	}
	if strings.Contains(upper, "NOT DELETED") {
		return !containsFlag(msg.Flags, "\\Deleted")
	}
	if strings.Contains(upper, "UNSEEN") {
		return !containsFlag(msg.Flags, "\\Seen")
	}
	if strings.Contains(upper, "SEEN") {
		return containsFlag(msg.Flags, "\\Seen")
	}
	if strings.Contains(upper, "FLAGGED") {
		return containsFlag(msg.Flags, "\\Flagged")
	}
	if strings.Contains(upper, "DELETED") {
		return containsFlag(msg.Flags, "\\Deleted")
	}
	return true
}

// ---------------------------------------------------------------------------
// APPEND
// ---------------------------------------------------------------------------

func (s *IMAPSession) handleAppend(tag, args string) {
	folder, rest := parseFirstQuotedOrAtom(args)

	var flags []string
	rest = strings.TrimSpace(rest)
	if len(rest) > 0 && rest[0] == '(' {
		end := strings.Index(rest, ")")
		if end != -1 {
			flags = parseFlagList(rest[:end+1])
			rest = strings.TrimSpace(rest[end+1:])
		}
	}

	if len(rest) > 0 && rest[0] == '"' {
		endQ := strings.Index(rest[1:], "\"")
		if endQ != -1 {
			rest = strings.TrimSpace(rest[endQ+2:])
		}
	}

	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "{") {
		s.send("%s BAD Missing literal", tag)
		return
	}
	closeBrace := strings.Index(rest, "}")
	if closeBrace == -1 {
		s.send("%s BAD Malformed literal", tag)
		return
	}
	sizeStr := rest[1:closeBrace]
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
		s.send("%s BAD Read error", tag)
		return
	}
	s.reader.ReadString('\n')

	subject, body, date := parseRFC2822Full(string(buf), s.store.localLoc)

	err = s.store.AppendMessage(folder, date, subject, body, flags)
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
				if msgs == nil {
					msgs = []*VirtualMessage{}
				}
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

func buildFetchResponse(msg *VirtualMessage, dataItems string, includeUID bool) string {
	var parts []string

	items := strings.ToUpper(dataItems)
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
	} else if strings.Contains(items, "RFC822") && !strings.Contains(items, "RFC822.") {
		parts = append(parts, fmt.Sprintf("RFC822 {%d}\r\n%s", len(rfc2822), rfc2822))
	}

	if strings.Contains(items, "ENVELOPE") {
		parts = append(parts, "ENVELOPE "+buildEnvelope(msg))
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

func buildEnvelope(msg *VirtualMessage) string {
	return fmt.Sprintf(
		`("%s" "%s" (("orgmail" NIL "orgmail" "localhost")) (("orgmail" NIL "orgmail" "localhost")) (("orgmail" NIL "orgmail" "localhost")) (("user" NIL "user" "localhost")) NIL NIL NIL "<org-%d@localhost>")`,
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
				include = wanted[strings.ToUpper(strings.TrimSpace(line[:colonIdx]))]
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

func parseUIDSet(set string, msgs []*VirtualMessage) []uint32 {
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
	s = strings.Trim(strings.TrimSpace(s), "()")
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
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

	folders := store.FolderPaths()
	log.Printf("Loaded %s (timezone: %s), %d folders:", orgFile, store.localLoc, len(folders))
	for _, f := range folders {
		msgs := store.MessagesInFolder(f)
		if len(msgs) > 0 {
			log.Printf("  %-50s %d msg(s)", f, len(msgs))
		} else {
			log.Printf("  %-50s (container)", f)
		}
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()
	log.Printf("IMAP server on %s", listenAddr)

	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for range t.C {
			if store.CheckReload() {
				log.Println("Org file reloaded (external change)")
			}
		}
	}()

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
			if !strings.Contains(err.Error(), "use of closed") {
				log.Printf("Accept error: %v", err)
			}
			continue
		}
		log.Printf("Connection from %s", conn.RemoteAddr())
		go NewIMAPSession(conn, store).Run()
	}
}

// suppress unused import
var _ = sort.Strings