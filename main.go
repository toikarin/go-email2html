package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"flag"
	"html/template"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
)

const index = "email.html"

const html = `<html>
<body>
	<div>
		Date: {{.Date}}<br>
		From: {{.From}}<br>
		To: {{.To}}<br>
		Subject: {{.Subject}}<br>
	</div>

	{{if .Html}}
	<div>
	<hr>
	<div class="html-content"><iframe width="1280" height="720" src="{{.Html.Filename}}"></iframe></div>
	</div>
	{{end}}

	{{if .Text}}
	<div>
	<hr>
	<div class="text-content">{{.Text}}</div>
	</div>
	{{end}}

	{{if .Attachments}}
	<div>
	<hr>
	Attachments:<br><br>
	{{range .Attachments}}
	<a href="{{.Filename}}">{{.Filename}}</a><br>
	{{end}}
	</div>
	{{end}}
</body>
</html>`

type Email struct {
	Date        string
	From        string
	To          string
	Subject     string
	Html        *Attachment
	Text        string
	Attachments []Attachment
}

type Attachment struct {
	Data     []byte
	Filename string
}

func main() {
	outputdir := flag.String("dir", "", "output directory")
	flag.Parse()

	if *outputdir == "" {
		panic("dir not set")
	}

	// Read from stdin
	reader := bufio.NewReader(os.Stdin)

	msg, err := mail.ReadMessage(reader)
	if err != nil {
		panic(err)
	}

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		panic(err)
	}

	from, err := decodeRFC2047Word(msg.Header.Get("From"))
	if err != nil {
		panic(err)
	}

	to, err := decodeRFC2047Word(msg.Header.Get("To"))
	if err != nil {
		panic(err)
	}

	subject, err := decodeRFC2047Word(msg.Header.Get("Subject"))
	if err != nil {
		panic(err)
	}

	email := Email{
		Date:    msg.Header.Get("Date"),
		From:    from,
		To:      to,
		Subject: subject,
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		handleMultipart(&email, msg.Body, params["boundary"])
	} else {
		addContent(&email, msg.Header, msg.Body)
	}

	writeResult(&email, *outputdir)
}

type Header interface {
	Get(key string) string
}

func isPlainText(contentType string) bool {
	return strings.HasPrefix(contentType, "text/plain")
}

func isHtml(contentType string) bool {
	return strings.HasPrefix(contentType, "text/html")
}

func isMultipart(contentType string) bool {
	return strings.HasPrefix(contentType, "multipart/")
}

func handleMultipart(email *Email, r io.Reader, boundary string) {
	reader := multipart.NewReader(r, boundary)

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}

		if err != nil {
			panic(err)
		}

		addContent(email, part.Header, part)
	}
}

type charsetError string

func (e charsetError) Error() string {
	return "charset not supported: " + string(e)
}

var rfc2047Decoder = mime.WordDecoder{
	CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
		return nil, charsetError(charset)
	},
}

// copied from net/mail/message
func decodeRFC2047Word(s string) (string, error) {
	dec, err := rfc2047Decoder.DecodeHeader(s)
	if err == nil {
		return dec, nil
	}

	if _, ok := err.(charsetError); ok {
		return s, err
	}

	// Ignore invalid RFC 2047 encoded-word errors.
	return s, nil
}

func addContent(email *Email, header Header, r io.Reader) {
	if header.Get("Content-Transfer-Encoding") == "quoted-printable" {
		r = quotedprintable.NewReader(r)
	}

	data, err := ioutil.ReadAll(r)
	if err != nil {
		panic(err)
	}

	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		panic(err)
	}

	if isPlainText(mediaType) && email.Text == "" {
		email.Text = string(data)
		email.Text = strings.Replace(email.Text, "\n", "<br>\n", -1)
	} else if isHtml(mediaType) && email.Html == nil {
		html := string(data)
		email.Html = &Attachment{Data: []byte(html), Filename: "email-content.html"}
	} else if isMultipart(mediaType) {
		handleMultipart(email, bytes.NewReader(data), params["boundary"])
	} else {
		var attachmentData []byte

		// check for byte64 encoding
		if header.Get("Content-Transfer-Encoding") == "base64" {
			var err error
			attachmentData, err = base64.StdEncoding.DecodeString(string(data))
			if err != nil {
				panic(err)
			}
		} else {
			attachmentData = data
		}

		// figure out filename
		filename := getFilename(header.Get("Content-Disposition"))
		if filename == "" {
			if isPlainText(mediaType) {
				filename = "attachment.txt"
			} else if isHtml(mediaType) {
				filename = "attachment.html"
			} else {
				panic("don't know how to generate filename for " + mediaType)
			}
		}

		email.Attachments = append(email.Attachments, Attachment{Data: attachmentData, Filename: filename})
	}
}

func getFilename(contentDisposition string) string {
	params := strings.Split(contentDisposition, ";")
	if len(params) < 2 {
		return ""
	}

	filename := strings.Split(params[1], "=")
	if len(filename) < 2 {
		return ""
	}

	return strings.Trim(filename[1], "\"")
}

func writeResult(email *Email, outputdir string) {
	// remove old directory
	err := os.RemoveAll(outputdir)
	if err != nil {
		panic(err)
	}

	// (re)create directory
	err = os.Mkdir(outputdir, 0755)
	if err != nil {
		panic(err)
	}

	// change mode to 0755, mkdir does not set it correctly
	f, err := os.Open(outputdir)
	if err != nil {
		panic(err)
	}

	defer f.Close()

	f.Chmod(0755)

	// write template
	t := template.Must(template.New("html").Parse(html))
	f, err = os.Create(filepath.Join(outputdir, index))
	if err != nil {
		panic(err)
	}

	defer f.Close()

	t.Execute(f, email)
	f.Chmod(0660)

	// write attachments
	if email.Html != nil {
		write(*email.Html, outputdir)
	}

	for _, attachment := range email.Attachments {
		write(attachment, outputdir)
	}
}

func write(attachment Attachment, outputdir string) {
	f, err := os.Create(filepath.Join(outputdir, attachment.Filename))
	if err != nil {
		panic(err)
	}

	defer f.Close()

	f.Write(attachment.Data)
	f.Chmod(0660)
}
