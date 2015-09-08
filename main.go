package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
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
	<head>
		<script>
			function toggleHeaders() {
				var e = document.getElementById("headers-all")
				e.style.display = (e.style.display == 'none') ? 'block' : 'none';
			}
		</script>
	</head>

	<div>
		Date: {{.Date}}<br>
		From: {{.From}}<br>
		To: {{.To}}<br>
		Subject: {{.Subject}}<br>
	</div>

	<a onclick="toggleHeaders()" href="#">Toggle all headers</a><br><br>

	<div id="headers-all" style="display: none">
		{{range $key, $val := .Headers}}
		{{range $v := $val}}
		{{$key}}: {{$v}}<br>
		{{end}}
		{{end}}
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
	Headers     map[string][]string
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
		fmt.Println("dir not set")
		os.Exit(1)
	}

	// Read from stdin
	err := handleMessage(bufio.NewReader(os.Stdin), *outputdir)
	if err != nil {
		fmt.Printf("Fatal error: %s\n", err)
		os.Exit(1)
	}
}

func handleMessage(reader io.Reader, outputdir string) error {
	msg, err := mail.ReadMessage(reader)
	if err != nil {
		return err
	}

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		return err
	}

	from, err := decodeRFC2047Word(msg.Header.Get("From"))
	if err != nil {
		return err
	}

	to, err := decodeRFC2047Word(msg.Header.Get("To"))
	if err != nil {
		return err
	}

	subject, err := decodeRFC2047Word(msg.Header.Get("Subject"))
	if err != nil {
		return err
	}

	// decode headers
	headers := make(map[string][]string)
	for k, values := range msg.Header {
		headers[k] = make([]string, len(values))

		for i, v := range values {
			headers[k][i], err = decodeRFC2047Word(v)
			if err != nil {
				return err
			}
		}
	}

	email := Email{
		Date:    msg.Header.Get("Date"),
		From:    from,
		To:      to,
		Subject: subject,
		Headers: headers,
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		if err := handleMultipart(&email, msg.Body, params["boundary"]); err != nil {
			return err
		}
	} else {
		if err := addContent(&email, msg.Header, msg.Body); err != nil {
			return err
		}
	}

	return writeResult(&email, outputdir)
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

func handleMultipart(email *Email, r io.Reader, boundary string) error {
	reader := multipart.NewReader(r, boundary)

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}

		if err != nil {
			return err
		}

		if err := addContent(email, part.Header, part); err != nil {
			return err
		}
	}

	return nil
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

func addContent(email *Email, header Header, r io.Reader) error {
	if header.Get("Content-Transfer-Encoding") == "quoted-printable" {
		r = quotedprintable.NewReader(r)
	}

	data, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}

	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		return err
	}

	if isPlainText(mediaType) && email.Text == "" {
		email.Text = string(data)
		email.Text = strings.Replace(email.Text, "\n", "<br>\n", -1)
	} else if isHtml(mediaType) && email.Html == nil {
		html := string(data)
		email.Html = &Attachment{Data: []byte(html), Filename: "email-content.html"}
	} else if isMultipart(mediaType) {
		if err := handleMultipart(email, bytes.NewReader(data), params["boundary"]); err != nil {
			return err
		}
	} else {
		var attachmentData []byte

		// check for byte64 encoding
		if header.Get("Content-Transfer-Encoding") == "base64" {
			var err error
			attachmentData, err = base64.StdEncoding.DecodeString(string(data))
			if err != nil {
				return err
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
				return errors.New("don't know how to generate filename for " + mediaType)
			}
		}

		email.Attachments = append(email.Attachments, Attachment{Data: attachmentData, Filename: filename})
	}

	return nil
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

func writeResult(email *Email, outputdir string) error {
	// remove old directory
	if err := os.RemoveAll(outputdir); err != nil {
		return err
	}

	// (re)create directory
	if err := os.Mkdir(outputdir, 0755); err != nil {
		return err
	}

	// change mode to 0755, mkdir does not set it correctly
	f, err := os.Open(outputdir)
	if err != nil {
		return err
	}

	defer f.Close()

	if err := f.Chmod(0755); err != nil {
		return err
	}

	// write template
	t := template.Must(template.New("html").Parse(html))
	f, err = os.Create(filepath.Join(outputdir, index))
	if err != nil {
		return err
	}

	defer f.Close()

	if err := t.Execute(f, email); err != nil {
		return err
	}

	if err := f.Chmod(0660); err != nil {
		return err
	}

	// write attachments
	if email.Html != nil {
		if err := write(*email.Html, outputdir); err != nil {
			return err
		}
	}

	for _, attachment := range email.Attachments {
		if err := write(attachment, outputdir); err != nil {
			return err
		}
	}

	return nil
}

func write(attachment Attachment, outputdir string) error {
	f, err := os.Create(filepath.Join(outputdir, attachment.Filename))
	if err != nil {
		return err
	}

	defer f.Close()

	if _, err = f.Write(attachment.Data); err != nil {
		return err
	}

	return f.Chmod(0660)
}
