package proxy

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"slices"
	"strings"
)

// Reading the billing fields ahead out of a multipart request body.
//
// The tension: routing and billing need the model first, but the order of the
// fields is the client's choice, and the model may sit behind tens of megabytes
// of image data. Two ways to resolve it:
//   - buffer the entire body and then parse it -- simple, but concurrent large
//     uploads exhaust memory;
//   - read ahead only until the model appears, then forward the bytes already
//     read joined to the remaining stream.
//
// The second is taken, with a hard cap on how far it will read ahead. Most
// clients put the small fields before the file, so in practice a few hundred
// bytes are read. If some client puts the model after a large file, the read
// stops at the cap and errors rather than buffering without bound.

// maxModelPeek caps the read-ahead. Passing it without seeing the model means
// the request shape is not acceptable. This is a defensive bound, not a feature
// limit.
const maxModelPeek = 1 << 20 // 1 MiB

// PeekedMultipart is the result: the fields billing needs, plus a body that can
// still be read in full.
type PeekedMultipart struct {
	Model string
	// Size and Quality are the axes a per-image rate is looked up by. They are
	// read here for the same reason the model is: the rate row has to be
	// resolved before the body is forwarded, so a value the card does not price
	// is refused before an image is generated. Absent fields stay empty and the
	// card's widest-matching row applies, which is what an upstream default
	// means.
	//
	// `n` is deliberately not among them. It used to be, as the quantity, and
	// it is not the quantity: how many images come back is counted from the
	// response (see imagesInResponse). Peeking a field nothing reads would only
	// suggest otherwise to the next reader.
	Size    string
	Quality string
	// prefix is the bytes already read, remainder is the unread rest, and
	// boundary is what separates the parts in both. Together they are the
	// original body; BodyFor is how it is put back together.
	prefix    []byte
	remainder io.Reader
	boundary  string
}

// peekFields are the small fields read ahead, in the spelling the OpenAI image
// endpoints use.
var peekFields = [...]string{"model", "size", "quality"}

// PeekMultipartModel reads the billing fields ahead out of a multipart stream.
//
// It does not consume the original stream: the bytes already read are kept in a
// bytes.Reader and joined back to the unread remainder through an
// io.MultiReader, so the upstream still receives the original body byte for
// byte.
//
// Only the model is required, and **the scan stops at the first file part once
// the model has been seen**. That bound is the whole point and it is easy to
// lose: Part.Close drains whatever it is closing, so a loop that kept going
// looking for the remaining small fields would read the upload itself, right up
// to the cap, on every ordinary request -- a megabyte buffered per concurrent
// edit where a few hundred bytes will do, and a megabyte read from the client
// before a single byte is forwarded. Measured at exactly maxModelPeek when that
// bound was missing.
//
// The price of stopping there is that a client putting `size` after the file is
// billed at the rate card's widest matching row rather than at the narrower one
// it asked for. That is the right trade: the alternative is buffering the file
// to find out, which is the thing this function exists not to do.
//
// If the model has *not* been seen by the first file part, the scan continues
// past it, because without a model there is no request at all -- that is the
// pathological case the cap was written for.
func PeekMultipartModel(body io.Reader, contentType string) (PeekedMultipart, error) {
	boundary, err := multipartBoundary(contentType)
	if err != nil {
		return PeekedMultipart{}, err
	}

	var peeked bytes.Buffer
	tee := io.TeeReader(io.LimitReader(body, maxModelPeek), &peeked)
	mr := multipart.NewReader(tee, boundary)

	found := map[string]string{}
	for len(found) < len(peekFields) {
		part, err := mr.NextPart()
		if err != nil {
			break // EOF or the read-ahead cap; treated as "not found" below
		}
		name := part.FormName()
		if slices.Contains(peekFields[:], name) && found[name] == "" {
			var sb strings.Builder
			if _, cErr := io.Copy(&sb, io.LimitReader(part, 1<<10)); cErr == nil {
				found[name] = strings.TrimSpace(sb.String())
			}
		}
		if part.FileName() != "" && found["model"] != "" {
			// Deliberately *not* closed: Part.Close drains what it closes, and
			// draining this one is precisely what must not happen. The reader
			// is abandoned here anyway -- the body is rebuilt below from the
			// bytes already teed plus the untouched remainder, so nothing
			// downstream depends on this part having been consumed.
			break
		}
		_ = part.Close()
	}

	if found["model"] == "" {
		return PeekedMultipart{}, fmt.Errorf("proxy: no model field found in multipart body (or it appears after the first %d bytes)", maxModelPeek)
	}
	// bytes already read plus the remaining stream equals the original body
	return PeekedMultipart{
		Model:     found["model"],
		Size:      found["size"],
		Quality:   found["quality"],
		prefix:    peeked.Bytes(),
		remainder: body,
		boundary:  boundary,
	}, nil
}

// BodyFor is the request body with the model field rewritten to the name the
// chosen upstream knows this model by.
//
// Every other endpoint gets that substitution from RewriteRequest, and this one
// could not: its body is a stream with an upload in it, and rewriting a
// multipart stream in flight was judged not worth it. The price was a rule that
// applied on one endpoint and nowhere else -- the route's upstream model id had
// to equal the second segment of the public slug -- which is the one place in
// the gateway where a route did not get to say what its upstream calls the
// model.
//
// It costs less than it looked, because the field is always in the bytes
// already read: the peek does not return until it has seen the model, so the
// substitution happens in a buffer that is in hand, and the upload itself is
// still never touched or buffered. Only the prefix is rebuilt; the remainder is
// joined back unread.
//
// A failure to locate the field is an error rather than a fall-back to the
// original bytes. Falling back would send a request naming a model this
// upstream may not have, and bill it against the one the caller asked for.
func (p *PeekedMultipart) BodyFor(upstreamModel string) (io.Reader, error) {
	if p.remainder == nil {
		// Single use, and it says so rather than quietly truncating. The
		// remainder is a stream: the first body handed out drains it, so a
		// second call would return the small fields followed by an exhausted
		// reader -- a well-formed prefix and no upload, which an upstream
		// answers with a confusing 400 rather than anything that points here.
		return nil, fmt.Errorf("proxy: the multipart body has already been handed out; it can only be read once")
	}
	remainder := p.remainder
	p.remainder = nil
	prefix := p.prefix
	if upstreamModel != "" && upstreamModel != p.Model {
		rewritten, ok := rewriteMultipartModel(prefix, p.boundary, upstreamModel)
		if !ok {
			return nil, fmt.Errorf("proxy: could not rewrite the model field of the multipart body")
		}
		prefix = rewritten
	}
	return io.MultiReader(bytes.NewReader(prefix), remainder), nil
}

// rewriteMultipartModel replaces the value of the `model` field in a multipart
// prefix, leaving every other byte where it was.
//
// It walks the parts rather than searching for the value, because the value is
// an ordinary string that can appear inside another field -- a prompt naming
// the model would be enough -- and replacing the wrong occurrence would corrupt
// the request in a way nothing downstream could notice.
func rewriteMultipartModel(prefix []byte, boundary, upstreamModel string) ([]byte, bool) {
	delim := []byte("--" + boundary)
	sep := append([]byte("\r\n"), delim...)
	for pos := 0; ; {
		i := bytes.Index(prefix[pos:], delim)
		if i < 0 {
			return nil, false
		}
		start := pos + i + len(delim)
		if !bytes.HasPrefix(prefix[start:], []byte("\r\n")) {
			// The closing delimiter, or a boundary-looking run inside a value.
			pos = start
			continue
		}
		headerStart := start + 2
		hEnd := bytes.Index(prefix[headerStart:], []byte("\r\n\r\n"))
		if hEnd < 0 {
			return nil, false
		}
		bodyStart := headerStart + hEnd + 4
		next := bytes.Index(prefix[bodyStart:], sep)
		if next < 0 {
			return nil, false
		}
		bodyEnd := bodyStart + next
		if isModelPart(prefix[headerStart : headerStart+hEnd]) {
			out := make([]byte, 0, len(prefix)-(bodyEnd-bodyStart)+len(upstreamModel))
			out = append(out, prefix[:bodyStart]...)
			out = append(out, upstreamModel...)
			out = append(out, prefix[bodyEnd:]...)
			return out, true
		}
		pos = bodyEnd
	}
}

// isModelPart reports whether a part's headers describe the `model` form field.
//
// A part carrying a filename is a file however it is named, and must never be
// mistaken for the field: rewriting an upload would replace its first bytes
// with a model id.
func isModelPart(header []byte) bool {
	for _, line := range bytes.Split(header, []byte("\r\n")) {
		name, value, ok := bytes.Cut(line, []byte(":"))
		if !ok || !strings.EqualFold(strings.TrimSpace(string(name)), "content-disposition") {
			continue
		}
		_, params, err := mime.ParseMediaType(strings.TrimSpace(string(value)))
		if err != nil {
			return false
		}
		return params["name"] == "model" && params["filename"] == ""
	}
	return false
}

// multipartBoundary pulls the boundary out of the Content-Type.
func multipartBoundary(contentType string) (string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("proxy: invalid Content-Type: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return "", fmt.Errorf("proxy: expected multipart, got %s", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", fmt.Errorf("proxy: multipart is missing its boundary")
	}
	return boundary, nil
}
