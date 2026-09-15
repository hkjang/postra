package application

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"sort"
	"strings"

	"postra/internal/domain"
)

type mimeEntity struct {
	header textproto.MIMEHeader
	body   []byte
}

func multiEntity(subtype string, children []mimeEntity) (mimeEntity, error) {
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	for _, child := range children {
		part, err := writer.CreatePart(child.header)
		if err != nil {
			return mimeEntity{}, err
		}
		if _, err = part.Write(child.body); err != nil {
			return mimeEntity{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return mimeEntity{}, err
	}
	return mimeEntity{header: textproto.MIMEHeader{"Content-Type": {mime.FormatMediaType("multipart/"+subtype, map[string]string{"boundary": writer.Boundary()})}}, body: out.Bytes()}, nil
}

func buildMIMEWithAttachments(acc *domain.MailAccount, v *domain.DraftVersion, msgID, inReplyTo, references string, data map[string][]byte) ([]byte, error) {
	base, err := buildMIME(acc, v, msgID, inReplyTo, references)
	if err != nil {
		return nil, err
	}
	message, err := mail.ReadMessage(bytes.NewReader(base))
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(message.Body)
	if err != nil {
		return nil, err
	}
	entity := mimeEntity{header: textproto.MIMEHeader{}, body: body}
	for _, name := range []string{"Content-Type", "Content-Transfer-Encoding"} {
		if value := message.Header.Get(name); value != "" {
			entity.header.Set(name, value)
			delete(message.Header, name)
		}
	}
	inline, regular := []mimeEntity{}, []mimeEntity{}
	for _, attachment := range v.Attachments {
		content, ok := data[attachment.ID]
		if !ok {
			return nil, userErrf("attachment content unavailable")
		}
		disposition := "attachment"
		if attachment.Inline {
			disposition = "inline"
		}
		header := textproto.MIMEHeader{"Content-Type": {mime.FormatMediaType(attachment.MIMEType, map[string]string{"name": attachment.Name})}, "Content-Disposition": {mime.FormatMediaType(disposition, map[string]string{"filename": attachment.Name})}, "Content-Transfer-Encoding": {"base64"}}
		if attachment.Inline {
			header.Set("Content-ID", "<"+attachment.ContentID+">")
		}
		encoded := base64.StdEncoding.EncodeToString(content)
		var wrapped strings.Builder
		for len(encoded) > 76 {
			wrapped.WriteString(encoded[:76] + "\r\n")
			encoded = encoded[76:]
		}
		wrapped.WriteString(encoded + "\r\n")
		part := mimeEntity{header: header, body: []byte(wrapped.String())}
		if attachment.Inline {
			inline = append(inline, part)
		} else {
			regular = append(regular, part)
		}
	}
	if len(inline) > 0 {
		entity, err = multiEntity("related", append([]mimeEntity{entity}, inline...))
		if err != nil {
			return nil, err
		}
	}
	if len(regular) > 0 {
		entity, err = multiEntity("mixed", append([]mimeEntity{entity}, regular...))
		if err != nil {
			return nil, err
		}
	}
	for key, value := range entity.header {
		message.Header[key] = value
	}
	var result bytes.Buffer
	keys := make([]string, 0, len(message.Header))
	for key := range message.Header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, value := range message.Header[key] {
			fmt.Fprintf(&result, "%s: %s\r\n", key, value)
		}
	}
	result.WriteString("\r\n")
	result.Write(entity.body)
	return result.Bytes(), nil
}
