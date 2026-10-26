package hostsetup

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/nodeproto"
)

// keyComment is the comment on the generated key pair.
const keyComment = "pbp"

// EnsureKey returns the layout's SSH public key ("ssh-ed25519 AAAA… pbp"),
// generating an ed25519 pair in OpenSSH format when no private key exists.
// An existing private key is never replaced; a missing .pub is re-derived
// from it. created reports whether a new pair was generated.
func EnsureKey(l Layout) (pub string, created bool, err error) {
	// #nosec G304 -- the layout's own key path
	priv, err := os.ReadFile(l.KeyPath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return generateKey(l)
	case err != nil:
		return "", false, fmt.Errorf("read SSH key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		return "", false, fmt.Errorf("existing SSH key %s: %w (setup never replaces it; use an unencrypted ed25519 key)", l.KeyPath(), err)
	}
	if t := signer.PublicKey().Type(); t != ssh.KeyAlgoED25519 {
		return "", false, fmt.Errorf("existing SSH key %s is %s; the node needs ssh-ed25519 (setup never replaces it)", l.KeyPath(), t)
	}
	derived := authorizedLine(signer.PublicKey())
	// #nosec G304 -- the layout's own key path
	have, err := os.ReadFile(l.PubKeyPath())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := l.WriteSecretFile(l.PubKeyPath(), []byte(derived+"\n")); err != nil {
			return "", false, err
		}
		return derived, false, nil
	case err != nil:
		return "", false, fmt.Errorf("read SSH public key: %w", err)
	}
	norm, err := nodeproto.ParsePubKey(string(have))
	if err != nil {
		return "", false, fmt.Errorf("%s: %w", l.PubKeyPath(), err)
	}
	if keyPart(norm) != keyPart(derived) {
		return "", false, fmt.Errorf("%s does not match %s; remove the .pub and run pbp setup again to re-derive it", l.PubKeyPath(), l.KeyPath())
	}
	return norm, false, nil
}

func generateKey(l Layout) (string, bool, error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", false, fmt.Errorf("generate SSH key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(sk, keyComment)
	if err != nil {
		return "", false, fmt.Errorf("encode SSH key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pk)
	if err != nil {
		return "", false, fmt.Errorf("encode SSH public key: %w", err)
	}
	if err := l.WriteSecretFile(l.KeyPath(), pem.EncodeToMemory(block)); err != nil {
		return "", false, err
	}
	pub := authorizedLine(sshPub)
	if err := l.WriteSecretFile(l.PubKeyPath(), []byte(pub+"\n")); err != nil {
		return "", false, err
	}
	return pub, true, nil
}

// authorizedLine renders "ssh-ed25519 <base64> pbp".
func authorizedLine(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " " + keyComment
}

// keyPart is "<type> <base64>" without the comment.
func keyPart(line string) string {
	f := strings.Fields(line)
	if len(f) < 2 {
		return line
	}
	return f[0] + " " + f[1]
}
