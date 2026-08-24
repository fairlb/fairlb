package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// AWS Signature Version 4.
//
// The signing itself is the vendor's own implementation rather than a
// reimplementation here. That is not a convenience preference: a signature is
// either byte-exact or rejected, the canonicalisation rules have a dozen edge
// cases around header folding and path escaping, and a bug in any of them
// produces an authentication failure indistinguishable from a wrong key. There
// is no partial credit to be had from writing it out again.

// Header names the signature adds. They are named here because two things have
// to agree about them: the request, and the list that says exactly which
// headers may leave.
const (
	amzDateHeader          = "X-Amz-Date"
	amzContentSHAHeader    = "X-Amz-Content-Sha256"
	amzSecurityTokenHeader = "X-Amz-Security-Token"
)

// awsCredential is the stored credential shape for a signing provider.
//
// A JSON object rather than a joined string. An access key pair is two values,
// three with a temporary credential, and joining them on a delimiter means
// choosing one that cannot occur in a secret access key -- a bet on a character
// set the vendor never promised. It also makes "you pasted only half of it" a
// diagnosable state instead of a signature that silently fails.
type awsCredential struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	// SessionToken is set for temporary credentials. It travels in its own
	// header and expires with them.
	SessionToken string `json:"session_token,omitempty"`
}

const awsCredentialShape = `a JSON object with access_key_id and secret_access_key`

func parseAWSCredential(secret string) (awsCredential, error) {
	var c awsCredential
	if err := decodeCredential(secret, &c, awsCredentialShape); err != nil {
		return awsCredential{}, err
	}
	if strings.TrimSpace(c.AccessKeyID) == "" || strings.TrimSpace(c.SecretAccessKey) == "" {
		return awsCredential{}, fmt.Errorf(
			"upstream: this provider's credential must be %s", awsCredentialShape)
	}
	return c, nil
}

// signAWS signs the request in place.
func signAWS(ctx context.Context, p Presentation) error {
	if !p.PayloadReadable {
		// A signature commits to a hash of the entire body, so a body that can
		// only be read once cannot be signed without buffering all of it. No
		// endpoint reached this way takes a streamed upload, so the honest
		// answer is to refuse rather than to buffer an unbounded request.
		return fmt.Errorf(
			"upstream: a signed request needs its whole body in hand, and this one is streamed")
	}
	region := p.Profile.SigningRegion()
	if region == "" {
		// The write path refuses this, so reaching it means a profile stored by
		// some other route. Failing here with the reason beats signing for an
		// empty region, which the service rejects with a message about the
		// credential.
		return fmt.Errorf("upstream: signing needs a region and this provider's profile has none")
	}
	cred, err := parseAWSCredential(p.Secret)
	if err != nil {
		return err
	}

	sum := sha256.Sum256(p.Payload)
	payloadHash := hex.EncodeToString(sum[:])
	// Set before signing so the hash is itself a signed header. Sending it
	// unsigned would let anything between here and the service swap the body
	// for one whose hash the header no longer matches, without breaking the
	// signature over the headers.
	p.HTTP.Header.Set(amzContentSHAHeader, payloadHash)

	// A fresh signer per request rather than a shared one: it holds no
	// connection state, construction is a struct literal, and a shared instance
	// would be one more thing that has to be safe under the concurrency this
	// data plane runs at.
	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, aws.Credentials{
		AccessKeyID:     cred.AccessKeyID,
		SecretAccessKey: cred.SecretAccessKey,
		SessionToken:    cred.SessionToken,
	}, p.HTTP, payloadHash, p.Profile.SigningService(), region, time.Now()); err != nil {
		return fmt.Errorf("upstream: signing the request: %w", err)
	}
	return nil
}
