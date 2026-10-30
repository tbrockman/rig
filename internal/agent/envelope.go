package agent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
)

// What the operator sends the agent is signed on the host and verified in the
// guest, by code, before the agent sees a word of it. The private key never
// enters the guest, so nothing there can forge an item; the agent is told that
// only rig's deliveries carry the operator's authority.
//
// An envelope file is two lines: the envelope's canonical JSON, then its
// ed25519 signature in base64. Canonical JSON never holds a raw newline, so the
// split is unambiguous, and a file can be read with cat. The signature covers
// SigContext and then the first line's exact bytes, and Open insists those
// bytes are what Seal would write, so one signature has one meaning.

// The kinds of envelope.
const (
	KindMessage     = "message"      // `rig agent send`
	KindAnswer      = "answer"       // `rig agent answer`, to the ask named by ID
	KindBriefUpdate = "brief-update" // `rig agent brief`: a unified diff against the brief the agent had
	KindBrief       = "brief"        // the brief itself, in brief.msg; never an inbox item
)

// IsInboxKind is whether kind may arrive through inbox.d. A brief may not:
// it is a standing file the runner and `rig brief` read, and its changes
// arrive as brief-update diffs, in order with everything else.
func IsInboxKind(kind string) bool {
	return kind == KindMessage || kind == KindAnswer || kind == KindBriefUpdate
}

// Envelope is one item from the operator to the agent.
type Envelope struct {
	V       int    `json:"v"`       // EnvelopeVersion
	Session string `json:"session"` // the agent session it is for (SessionKey)
	Seq     uint64 `json:"seq"`     // the host's counter for this VM; the guest accepts each only once, in order
	Time    string `json:"time"`    // RFC 3339, the host's clock
	Kind    string `json:"kind"`    // KindMessage, KindAnswer, KindBriefUpdate or KindBrief
	ID      string `json:"id"`      // a message's own id, or the ask an answer answers
	Body    string `json:"body"`
}

// EnvelopeVersion is the only version there is.
const EnvelopeVersion = 1

// SigContext separates rig's signatures from anything else the key might ever
// sign: a signature over these bytes cannot be mistaken for one over a
// different kind of document.
const SigContext = "rig agent operator envelope v1\x00"

// MaxEnvelope bounds what a verifier reads. A message is prose; anything
// larger is not one.
const MaxEnvelope = 1 << 20

// Why an item was refused. Wrapped with the detail, so errors.Is tells the
// kinds apart and the message says what was wrong.
var (
	ErrMalformed = errors.New("malformed")
	ErrSignature = errors.New("bad signature")
	ErrSession   = errors.New("wrong session")
	ErrReplay    = errors.New("sequence number already used")
)

var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidID is what an ask or message id may be: short, and safe as a file name.
func ValidID(id string) bool { return idRE.MatchString(id) }

func (e Envelope) canonical() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func (e Envelope) check() error {
	switch {
	case e.V != EnvelopeVersion:
		return fmt.Errorf("%w: version %d", ErrMalformed, e.V)
	case !IsInboxKind(e.Kind) && e.Kind != KindBrief:
		return fmt.Errorf("%w: kind %q", ErrMalformed, e.Kind)
	case !ValidID(e.ID):
		return fmt.Errorf("%w: id %q", ErrMalformed, e.ID)
	case e.Session == "":
		return fmt.Errorf("%w: no session", ErrMalformed)
	case e.Seq == 0:
		return fmt.Errorf("%w: no sequence number", ErrMalformed)
	}
	return nil
}

// Seal signs e and returns the file the guest reads.
func Seal(key ed25519.PrivateKey, e Envelope) ([]byte, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	payload, err := e.canonical()
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(key, append([]byte(SigContext), payload...))
	out := append(payload, '\n')
	out = append(out, base64.StdEncoding.EncodeToString(sig)...)
	return append(out, '\n'), nil
}

// Open checks data's signature against pub and returns the envelope it
// carries. Whether it is for this session, and whether it was seen before, are
// Admit's questions.
func Open(pub ed25519.PublicKey, data []byte) (Envelope, error) {
	var e Envelope
	if len(pub) != ed25519.PublicKeySize {
		return e, errors.New("no usable operator key")
	}
	if len(data) > MaxEnvelope {
		return e, fmt.Errorf("%w: %d bytes", ErrMalformed, len(data))
	}
	payload, rest, ok := bytes.Cut(data, []byte("\n"))
	rest = bytes.TrimSuffix(rest, []byte("\n"))
	if !ok || bytes.ContainsAny(rest, "\n") {
		return e, fmt.Errorf("%w: not an envelope and a signature", ErrMalformed)
	}
	sig, err := base64.StdEncoding.DecodeString(string(rest))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return e, fmt.Errorf("%w: unreadable signature", ErrMalformed)
	}
	if !ed25519.Verify(pub, append([]byte(SigContext), payload...), sig) {
		return e, ErrSignature
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	// Signed bytes the sealer would not have written are refused even with a
	// good signature: one signature, one reading.
	if again, err := e.canonical(); err != nil || !bytes.Equal(again, payload) {
		return Envelope{}, fmt.Errorf("%w: not canonical", ErrMalformed)
	}
	if err := e.check(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// Admit applies the replay rules to an opened envelope: it must be for
// session, and its sequence number greater than the last one accepted.
func (e Envelope) Admit(session string, last uint64) error {
	if e.Session != session {
		return fmt.Errorf("%w: for session %s, this run is %s", ErrSession, e.Session, session)
	}
	if e.Seq <= last {
		return fmt.Errorf("%w: #%d, and #%d was already accepted", ErrReplay, e.Seq, last)
	}
	return nil
}

// pkixEd25519 is the DER prefix of every ed25519 SubjectPublicKeyInfo (RFC
// 8410): the key's 32 bytes follow it. Written out rather than asking
// crypto/x509, which would more than double the guest build for one constant.
var pkixEd25519 = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}

// MarshalPublicKey is pub as a PEM "PUBLIC KEY", the form openssl reads.
func MarshalPublicKey(pub ed25519.PublicKey) []byte {
	der := append(append([]byte{}, pkixEd25519...), pub...)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// ParsePublicKey reads what MarshalPublicKey wrote.
func ParsePublicKey(b []byte) (ed25519.PublicKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PUBLIC KEY" {
		return nil, errors.New("not a PEM public key")
	}
	if len(blk.Bytes) != len(pkixEd25519)+ed25519.PublicKeySize || !bytes.HasPrefix(blk.Bytes, pkixEd25519) {
		return nil, errors.New("not an ed25519 public key")
	}
	return ed25519.PublicKey(bytes.Clone(blk.Bytes[len(pkixEd25519):])), nil
}
