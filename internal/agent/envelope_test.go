package agent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func sample() Envelope {
	return Envelope{V: EnvelopeVersion, Session: "sess-a", Seq: 7, Time: "2026-10-16T10:00:00Z",
		Kind: KindMessage, ID: "m7", Body: "Also run the benchmarks.\nThen stop."}
}

func TestEnvelopeRoundTrips(t *testing.T) {
	pub, priv := keypair(t)
	data, err := Seal(priv, sample())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte("\n")) != 2 {
		t.Errorf("an envelope file is two lines, got:\n%s", data)
	}
	got, err := Open(pub, data)
	if err != nil {
		t.Fatal(err)
	}
	if got != sample() {
		t.Errorf("got %+v, want %+v", got, sample())
	}
	if err := got.Admit("sess-a", 6); err != nil {
		t.Errorf("the next number in this session must be admitted: %v", err)
	}
}

// The property the whole design rests on: without the private key, nothing in
// the guest can produce a file the verifier accepts.
func TestEnvelopeFromAnotherKeyIsRefused(t *testing.T) {
	pub, _ := keypair(t)
	_, other := keypair(t)
	data, _ := Seal(other, sample())
	if _, err := Open(pub, data); !errors.Is(err, ErrSignature) {
		t.Errorf("a different key's signature must be refused, got %v", err)
	}
}

func TestTamperedBodyIsRefused(t *testing.T) {
	pub, priv := keypair(t)
	data, _ := Seal(priv, sample())
	forged := bytes.Replace(data, []byte("benchmarks"), []byte("rm -rf /work"), 1)
	if _, err := Open(pub, forged); !errors.Is(err, ErrSignature) {
		t.Errorf("an edited body must break the signature, got %v", err)
	}
}

func TestOtherSessionsAndUsedNumbersAreNotAdmitted(t *testing.T) {
	e := sample()
	if err := e.Admit("sess-b", 0); !errors.Is(err, ErrSession) {
		t.Errorf("a message for another session must be refused, got %v", err)
	}
	if err := e.Admit("sess-a", 7); !errors.Is(err, ErrReplay) {
		t.Errorf("the number already accepted must be refused, got %v", err)
	}
	if err := e.Admit("sess-a", 9); !errors.Is(err, ErrReplay) {
		t.Errorf("an older number must be refused, got %v", err)
	}
}

// One signature, one reading: signed bytes that are not what Seal writes are
// refused, so no two parsers can disagree about what was signed.
func TestOnlyCanonicalEnvelopesOpen(t *testing.T) {
	pub, priv := keypair(t)
	sign := func(payload string) []byte {
		sig := ed25519.Sign(priv, append([]byte(SigContext), payload...))
		return []byte(payload + "\n" + base64.StdEncoding.EncodeToString(sig) + "\n")
	}
	good, _ := Seal(priv, sample())
	payload, _, _ := strings.Cut(string(good), "\n")
	for name, p := range map[string]string{
		"spaced":        strings.Replace(payload, `"v":1`, `"v": 1`, 1),
		"unknown field": strings.Replace(payload, `{"v":1`, `{"admin":true,"v":1`, 1),
		"duplicate key": strings.Replace(payload, `"body":`, `"body":"x","body":`, 1),
		"other kind":    strings.Replace(payload, `"kind":"message"`, `"kind":"root"`, 1),
	} {
		if _, err := Open(pub, sign(p)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: want ErrMalformed, got %v", name, err)
		}
	}
	// The context is part of what is signed: a bare signature over the same
	// bytes, as some other use of the key might make, is not one of rig's.
	bare := ed25519.Sign(priv, []byte(payload))
	if _, err := Open(pub, []byte(payload+"\n"+base64.StdEncoding.EncodeToString(bare)+"\n")); !errors.Is(err, ErrSignature) {
		t.Errorf("a signature without the context must be refused, got %v", err)
	}
	for name, data := range map[string]string{"empty": "", "one line": payload, "three lines": string(good) + "x\n"} {
		if _, err := Open(pub, []byte(data)); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

func TestSealRefusesWhatOpenWould(t *testing.T) {
	_, priv := keypair(t)
	for name, mut := range map[string]func(*Envelope){
		"no session": func(e *Envelope) { e.Session = "" },
		"no seq":     func(e *Envelope) { e.Seq = 0 },
		"bad id":     func(e *Envelope) { e.ID = "../x" },
		"bad kind":   func(e *Envelope) { e.Kind = "shell" },
	} {
		e := sample()
		mut(&e)
		if _, err := Seal(priv, e); err == nil {
			t.Errorf("%s: sealed", name)
		}
	}
}

// operator.pub is the standard form, so openssl can read it, without the
// guest linking crypto/x509 to say so.
func TestPublicKeyIsPKIXAndRoundTrips(t *testing.T) {
	pub, _ := keypair(t)
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(der, append(append([]byte{}, pkixEd25519...), pub...)) {
		t.Fatal("the DER prefix is not the one x509 writes")
	}
	got, err := ParsePublicKey(MarshalPublicKey(pub))
	if err != nil || !got.Equal(pub) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := ParsePublicKey([]byte("ssh-ed25519 AAAA")); err == nil {
		t.Error("a non-PEM key must be refused")
	}
}

func TestOnlyMessagesAnswersAndBriefUpdatesComeThroughTheInbox(t *testing.T) {
	for kind, want := range map[string]bool{KindMessage: true, KindAnswer: true, KindBriefUpdate: true, KindBrief: false, "": false} {
		if IsInboxKind(kind) != want {
			t.Errorf("IsInboxKind(%q) = %v", kind, !want)
		}
	}
}
