// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package email

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/mail"
	"regexp"
	"sort"
	"strings"
)

type Email struct {
	BugID       string
	MessageID   string
	Link        string
	Subject     string
	From        string
	Cc          []string
	Body        string // text/plain part
	Patch       string // attached patch, if any
	Command     string // command to bot (#syz is stripped)
	CommandArgs string // arguments for the command
}

const commandPrefix = "#syz "

var groupsLinkRe = regexp.MustCompile("\nTo view this discussion on the web visit (https://groups\\.google\\.com/.*?)\\.(?:\r)?\n")

func Parse(r io.Reader, ownEmails []string) (*Email, error) {
	msg, err := mail.ReadMessage(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read email: %v", err)
	}
	from, err := msg.Header.AddressList("From")
	if err != nil {
		return nil, fmt.Errorf("failed to parse email header 'From': %v", err)
	}
	if len(from) == 0 {
		return nil, fmt.Errorf("failed to parse email header 'To': no senders")
	}
	// Ignore errors since To: header may not be present (we've seen such case).
	to, _ := msg.Header.AddressList("To")
	// AddressList fails if the header is not present.
	cc, _ := msg.Header.AddressList("Cc")
	bugID := ""
	var ccList []string
	ownAddrs := make(map[string]bool)
	for _, email := range ownEmails {
		ownAddrs[email] = true
		if addr, err := mail.ParseAddress(email); err == nil {
			ownAddrs[addr.Address] = true
		}
	}
	fromMe := false
	for _, addr := range from {
		cleaned, _, _ := RemoveAddrContext(addr.Address)
		if addr, err := mail.ParseAddress(cleaned); err == nil && ownAddrs[addr.Address] {
			fromMe = true
		}
	}
	for _, addr := range append(append(cc, to...), from...) {
		cleaned, context, _ := RemoveAddrContext(addr.Address)
		if addr, err := mail.ParseAddress(cleaned); err == nil {
			cleaned = addr.Address
		}
		if ownAddrs[cleaned] {
			if bugID == "" {
				bugID = context
			}
		} else {
			ccList = append(ccList, cleaned)
		}
	}
	ccList = MergeEmailLists(ccList)
	body, attachments, err := parseBody(msg.Body, msg.Header)
	if err != nil {
		return nil, err
	}
	bodyStr := string(body)
	patch, cmd, cmdArgs := "", "", ""
	if !fromMe {
		for _, a := range attachments {
			_, patch, _ = ParsePatch(string(a))
			if patch != "" {
				break
			}
		}
		if patch == "" {
			_, patch, _ = ParsePatch(bodyStr)
		}
		cmd, cmdArgs = extractCommand(body)
	}
	link := ""
	if match := groupsLinkRe.FindStringSubmatchIndex(bodyStr); match != nil {
		link = bodyStr[match[2]:match[3]]
	}
	email := &Email{
		BugID:       bugID,
		MessageID:   msg.Header.Get("Message-ID"),
		Link:        link,
		Subject:     msg.Header.Get("Subject"),
		From:        from[0].String(),
		Cc:          ccList,
		Body:        string(body),
		Patch:       patch,
		Command:     cmd,
		CommandArgs: cmdArgs,
	}
	return email, nil
}

// AddAddrContext embeds context into local part of the provided email address using '+'.
// Returns the resulting email address.
func AddAddrContext(email, context string) (string, error) {
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return "", fmt.Errorf("failed to parse %q as email: %v", email, err)
	}
	at := strings.IndexByte(addr.Address, '@')
	if at == -1 {
		return "", fmt.Errorf("failed to parse %q as email: no @", email)
	}
	result := addr.Address[:at] + "+" + context + addr.Address[at:]
	if addr.Name != "" {
		addr.Address = result
		result = addr.String()
	}
	return result, nil
}

// RemoveAddrContext extracts context after '+' from the local part of the provided email address.
// Returns address without the context and the context.
func RemoveAddrContext(email string) (string, string, error) {
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse %q as email: %v", email, err)
	}
	at := strings.IndexByte(addr.Address, '@')
	if at == -1 {
		return "", "", fmt.Errorf("failed to parse %q as email: no @", email)
	}
	plus := strings.LastIndexByte(addr.Address[:at], '+')
	if plus == -1 {
		return email, "", nil
	}
	context := addr.Address[plus+1 : at]
	addr.Address = addr.Address[:plus] + addr.Address[at:]
	return addr.String(), context, nil
}

func CanonicalEmail(email string) string {
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return email
	}
	at := strings.IndexByte(addr.Address, '@')
	if at == -1 {
		return email
	}
	if plus := strings.IndexByte(addr.Address[:at], '+'); plus != -1 {
		addr.Address = addr.Address[:plus] + addr.Address[at:]
	}
	return strings.ToLower(addr.Address)
}

// extractCommand extracts command to syzbot from email body.
// Commands are of the following form:
// ^#syz cmd args...
func extractCommand(body []byte) (cmd, args string) {
	cmdPos := bytes.Index(append([]byte{'\n'}, body...), []byte("\n"+commandPrefix))
	if cmdPos == -1 {
		return
	}
	cmdPos += len(commandPrefix)
	for cmdPos < len(body) && body[cmdPos] == ' ' {
		cmdPos++
	}
	cmdEnd := bytes.IndexByte(body[cmdPos:], '\n')
	if cmdEnd == -1 {
		cmdEnd = len(body) - cmdPos
	}
	if cmdEnd1 := bytes.IndexByte(body[cmdPos:], '\r'); cmdEnd1 != -1 && cmdEnd1 < cmdEnd {
		cmdEnd = cmdEnd1
	}
	if cmdEnd1 := bytes.IndexByte(body[cmdPos:], ' '); cmdEnd1 != -1 && cmdEnd1 < cmdEnd {
		cmdEnd = cmdEnd1
	}
	cmd = string(body[cmdPos : cmdPos+cmdEnd])
	// Some email clients split text emails at 80 columns are the transformation is irrevesible.
	// We try hard to restore what was there before.
	// For "test:" command we know that there must be 2 tokens without spaces.
	// For "fix:"/"dup:" we need a whole non-empty line of text.
	switch cmd {
	case "test:":
		args = extractArgsTokens(body[cmdPos+cmdEnd:], 2)
	case "test_5_arg_cmd":
		args = extractArgsTokens(body[cmdPos+cmdEnd:], 5)
	case "fix:", "dup:":
		args = extractArgsLine(body[cmdPos+cmdEnd:])
	}
	return
}

func extractArgsTokens(body []byte, num int) string {
	var args []string
	for pos := 0; len(args) < num && pos < len(body); {
		lineEnd := bytes.IndexByte(body[pos:], '\n')
		if lineEnd == -1 {
			lineEnd = len(body) - pos
		}
		line := strings.TrimSpace(string(body[pos : pos+lineEnd]))
		for {
			line1 := strings.Replace(line, "  ", " ", -1)
			if line == line1 {
				break
			}
			line = line1
		}
		if line != "" {
			args = append(args, strings.Split(line, " ")...)
		}
		pos += lineEnd + 1
	}
	return strings.TrimSpace(strings.Join(args, " "))
}

func extractArgsLine(body []byte) string {
	pos := 0
	for pos < len(body) && (body[pos] == ' ' || body[pos] == '\t' ||
		body[pos] == '\n' || body[pos] == '\r') {
		pos++
	}
	lineEnd := bytes.IndexByte(body[pos:], '\n')
	if lineEnd == -1 {
		lineEnd = len(body) - pos
	}
	return strings.TrimSpace(string(body[pos : pos+lineEnd]))
}

func parseBody(r io.Reader, headers mail.Header) (body []byte, attachments [][]byte, err error) {
	// git-send-email sends emails without Content-Type, let's assume it's text.
	mediaType := "text/plain"
	var params map[string]string
	if contentType := headers.Get("Content-Type"); contentType != "" {
		mediaType, params, err = mime.ParseMediaType(headers.Get("Content-Type"))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse email header 'Content-Type': %v", err)
		}
	}
	// Note: mime package handles quoted-printable internally.
	if strings.ToLower(headers.Get("Content-Transfer-Encoding")) == "base64" {
		r = base64.NewDecoder(base64.StdEncoding, r)
	}
	disp, _, _ := mime.ParseMediaType(headers.Get("Content-Disposition"))
	if disp == "attachment" {
		attachment, err := ioutil.ReadAll(r)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read email body: %v", err)
		}
		return nil, [][]byte{attachment}, nil
	}
	if mediaType == "text/plain" {
		body, err := ioutil.ReadAll(r)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read email body: %v", err)
		}
		return body, nil, nil
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, nil, nil
	}
	mr := multipart.NewReader(r, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return body, attachments, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse MIME parts: %v", err)
		}
		body1, attachments1, err1 := parseBody(p, mail.Header(p.Header))
		if err1 != nil {
			return nil, nil, err1
		}
		if body == nil {
			body = body1
		}
		attachments = append(attachments, attachments1...)
	}
}

// MergeEmailLists merges several email lists removing duplicates and invalid entries.
func MergeEmailLists(lists ...[]string) []string {
	const (
		maxEmailLen = 1000
		maxEmails   = 50
	)
	merged := make(map[string]bool)
	for _, list := range lists {
		for _, email := range list {
			addr, err := mail.ParseAddress(email)
			if err != nil || len(addr.Address) > maxEmailLen {
				continue
			}
			merged[addr.Address] = true
		}
	}
	var result []string
	for e := range merged {
		result = append(result, e)
	}
	sort.Strings(result)
	if len(result) > maxEmails {
		result = result[:maxEmails]
	}
	return result
}
