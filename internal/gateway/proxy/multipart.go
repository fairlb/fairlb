package proxy

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strings"
)

// Reading the model name ahead out of a multipart request body.
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

// PeekedMultipart is the result: the model name plus a body that can still be
// read in full.
type PeekedMultipart struct {
	Model string
	// Body is the bytes already read joined to the remaining stream, and is
	// used as the complete request body when forwarding.
	Body io.Reader
}

// PeekMultipartModel reads the model field ahead out of a multipart stream.
//
// It does not consume the original stream: the bytes already read are kept in a
// bytes.Reader and joined back to the unread remainder through an
// io.MultiReader, so the upstream still receives the original body byte for
// byte.
func PeekMultipartModel(body io.Reader, contentType string) (PeekedMultipart, error) {
	boundary, err := multipartBoundary(contentType)
	if err != nil {
		return PeekedMultipart{}, err
	}

	var peeked bytes.Buffer
	tee := io.TeeReader(io.LimitReader(body, maxModelPeek), &peeked)
	mr := multipart.NewReader(tee, boundary)

	model := ""
	for model == "" {
		part, err := mr.NextPart()
		if err != nil {
			break // EOF or the read-ahead cap; treated as "not found" below
		}
		if part.FormName() == "model" {
			var sb strings.Builder
			if _, cErr := io.Copy(&sb, io.LimitReader(part, 1<<10)); cErr == nil {
				model = strings.TrimSpace(sb.String())
			}
		}
		_ = part.Close()
	}

	if model == "" {
		return PeekedMultipart{}, fmt.Errorf("proxy: no model field found in multipart body (or it appears after the first %d bytes)", maxModelPeek)
	}
	// bytes already read plus the remaining stream equals the original body
	return PeekedMultipart{
		Model: model,
		Body:  io.MultiReader(bytes.NewReader(peeked.Bytes()), body),
	}, nil
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
