package events

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"

	"github.com/nats-io/nkeys"
)

func fixedNATSRoleNames() []string {
	return []string{"account-outbox-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"}
}

func fixedNATSV2RoleNames() []string {
	return []string{"account-outbox-relay", "account-approval-relay", "billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"}
}

const natsIdentityOverlapMaximum = 7 * 24 * time.Hour

type natsIdentityArtifact struct {
	version          int
	keys             map[string]string
	previousV1Digest string
	overlapExpiresAt time.Time
}

// NATSAuthorizationArtifactFiles is the public, non-secret evidence used by
// the authorization Relay readiness probe. Trusted digests are projected
// independently from the public JSON files. PrivateIdentityFiles is used only
// to prove that the public artifacts were not mounted from the credential
// directory; its contents are never opened here.
type NATSAuthorizationArtifactFiles struct {
	CurrentFile          string
	CurrentSHA256        string
	PreviousFile         string
	PreviousSHA256       string
	PrivateIdentityFiles []string
}

// Validate checks the currently configured NATS authorization overlap at one
// trusted UTC instant. A v2 artifact without a previous v1 remains valid after
// its historical overlap deadline; a still-configured previous v1 expires at
// the exact deadline and makes the dedicated readiness probe fail closed.
func (files NATSAuthorizationArtifactFiles) Validate(now time.Time) error {
	if now.Location() != time.UTC || strings.TrimSpace(files.CurrentFile) == "" || strings.TrimSpace(files.CurrentSHA256) == "" {
		return ErrNATSResources
	}
	if (strings.TrimSpace(files.PreviousFile) == "") != (strings.TrimSpace(files.PreviousSHA256) == "") {
		return ErrNATSResources
	}
	publicPaths := []string{files.CurrentFile}
	if files.PreviousFile != "" {
		publicPaths = append(publicPaths, files.PreviousFile)
	}
	publicDirectory := filepath.Dir(filepath.Clean(files.CurrentFile))
	expectedPublicFiles := map[string]bool{filepath.Base(files.CurrentFile): true}
	for _, publicPath := range publicPaths {
		if filepath.Dir(filepath.Clean(publicPath)) != publicDirectory || !isolatedNATSPublicArtifactPath(publicPath, files.PrivateIdentityFiles) {
			return ErrNATSResources
		}
		expectedPublicFiles[filepath.Base(publicPath)] = true
	}
	entries, err := os.ReadDir(publicDirectory)
	if err != nil || len(entries) != len(expectedPublicFiles) {
		return ErrNATSResources
	}
	for _, entry := range entries {
		if !expectedPublicFiles[entry.Name()] || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return ErrNATSResources
		}
	}
	current, currentInfo, err := readNATSPublicArtifact(files.CurrentFile, files.CurrentSHA256)
	if err != nil {
		return ErrNATSResources
	}
	primary, err := parseNATSIdentityArtifact(current)
	if err != nil || primary.version != 2 {
		return ErrNATSResources
	}
	var previous []byte
	if files.PreviousFile != "" {
		var previousInfo os.FileInfo
		previous, previousInfo, err = readNATSPublicArtifact(files.PreviousFile, files.PreviousSHA256)
		if err != nil || os.SameFile(currentInfo, previousInfo) {
			return ErrNATSResources
		}
	}
	_, err = renderNATSAuthorizationAt(current, previous, now)
	if err != nil {
		return ErrNATSResources
	}
	return nil
}

func isolatedNATSPublicArtifactPath(publicPath string, privateFiles []string) bool {
	cleanPublic := filepath.Clean(publicPath)
	if !filepath.IsAbs(publicPath) || cleanPublic != publicPath {
		return false
	}
	for _, privateFile := range privateFiles {
		if strings.TrimSpace(privateFile) == "" {
			continue
		}
		cleanPrivate := filepath.Clean(privateFile)
		if !filepath.IsAbs(privateFile) || cleanPrivate != privateFile {
			return false
		}
		privateDirectory := filepath.Dir(cleanPrivate)
		relative, err := filepath.Rel(privateDirectory, cleanPublic)
		if err != nil || relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return false
		}
	}
	return true
}

func readNATSPublicArtifact(path, expected string) ([]byte, os.FileInfo, error) {
	if len(expected) != 64 || strings.ToLower(expected) != expected {
		return nil, nil, ErrNATSResources
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return nil, nil, ErrNATSResources
	}
	resolved, err := filepath.EvalSymlinks(path)
	info, statErr := os.Lstat(path)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || err != nil || resolved != path || statErr != nil || !info.Mode().IsRegular() || info.Size() > 32<<10 || (runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0) {
		return nil, nil, ErrNATSResources
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, ErrNATSResources
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() > 32<<10 || (runtime.GOOS != "windows" && opened.Mode().Perm()&0022 != 0) {
		return nil, nil, ErrNATSResources
	}
	b, err := io.ReadAll(io.LimitReader(f, (32<<10)+1))
	if err != nil || len(b) > 32<<10 {
		return nil, nil, ErrNATSResources
	}
	digest := sha256.Sum256(b)
	if hex.EncodeToString(digest[:]) != expected {
		return nil, nil, ErrNATSResources
	}
	return b, opened, nil
}

// parseNATSPublicKeys accepts only the closed public identity artifact, never
// caller-supplied permissions. Token parsing rejects duplicate JSON keys.
func parseNATSPublicKeys(raw []byte) (map[string]string, error) {
	artifact, err := parseNATSIdentityArtifact(raw)
	if err != nil {
		return nil, err
	}
	return artifact.keys, nil
}

func parseNATSIdentityArtifact(raw []byte) (natsIdentityArtifact, error) {
	if len(raw) == 0 || len(raw) > 32<<10 {
		return natsIdentityArtifact{}, ErrNATSResources
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return natsIdentityArtifact{}, ErrNATSResources
	}
	seen := map[string]bool{}
	keys := map[string]string{}
	artifact := natsIdentityArtifact{keys: keys}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return natsIdentityArtifact{}, ErrNATSResources
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return natsIdentityArtifact{}, ErrNATSResources
		}
		seen[name] = true
		switch name {
		case "version":
			var version int
			if d.Decode(&version) != nil || (version != 1 && version != 2) {
				return natsIdentityArtifact{}, ErrNATSResources
			}
			artifact.version = version
		case "purpose":
			var purpose string
			if d.Decode(&purpose) != nil || purpose != "pinvou-nats-service-identities" {
				return natsIdentityArtifact{}, ErrNATSResources
			}
		case "public_keys":
			start, err := d.Token()
			if err != nil || start != json.Delim('{') {
				return natsIdentityArtifact{}, ErrNATSResources
			}
			for d.More() {
				token, err := d.Token()
				if err != nil {
					return natsIdentityArtifact{}, ErrNATSResources
				}
				role, ok := token.(string)
				if !ok {
					return natsIdentityArtifact{}, ErrNATSResources
				}
				if _, exists := keys[role]; exists {
					return natsIdentityArtifact{}, ErrNATSResources
				}
				var public string
				if d.Decode(&public) != nil || !nkeys.IsValidPublicUserKey(public) {
					return natsIdentityArtifact{}, ErrNATSResources
				}
				keys[role] = public
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return natsIdentityArtifact{}, ErrNATSResources
			}
		case "previous_v1_sha256":
			if d.Decode(&artifact.previousV1Digest) != nil || len(artifact.previousV1Digest) != 64 || strings.ToLower(artifact.previousV1Digest) != artifact.previousV1Digest {
				return natsIdentityArtifact{}, ErrNATSResources
			}
			if _, err := hex.DecodeString(artifact.previousV1Digest); err != nil {
				return natsIdentityArtifact{}, ErrNATSResources
			}
		case "overlap_expires_at":
			var value string
			if d.Decode(&value) != nil {
				return natsIdentityArtifact{}, ErrNATSResources
			}
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339) != value {
				return natsIdentityArtifact{}, ErrNATSResources
			}
			artifact.overlapExpiresAt = parsed
		default:
			return natsIdentityArtifact{}, ErrNATSResources
		}
	}
	end, err := d.Token()
	roles := fixedNATSRoleNames()
	expectedFields := 3
	if artifact.version == 2 {
		roles = fixedNATSV2RoleNames()
		expectedFields = 5
	}
	if err != nil || end != json.Delim('}') || artifact.version == 0 || len(seen) != expectedFields || len(keys) != len(roles) || d.Decode(new(any)) != io.EOF {
		return natsIdentityArtifact{}, ErrNATSResources
	}
	unique := map[string]bool{}
	for _, role := range roles {
		public := keys[role]
		if public == "" || unique[public] {
			return natsIdentityArtifact{}, ErrNATSResources
		}
		unique[public] = true
	}
	if artifact.version == 1 && (artifact.previousV1Digest != "" || !artifact.overlapExpiresAt.IsZero()) {
		return natsIdentityArtifact{}, ErrNATSResources
	}
	if artifact.version == 2 && (artifact.previousV1Digest == "" || artifact.overlapExpiresAt.IsZero()) {
		return natsIdentityArtifact{}, ErrNATSResources
	}
	return artifact, nil
}

type natsPermissions struct {
	Publish   []string `json:"publish"`
	Subscribe []string `json:"subscribe"`
}
type natsSubjectPermission struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny,omitempty"`
}
type natsAuthorizedUser struct {
	NKey        string          `json:"nkey"`
	Permissions natsPermissions `json:"permissions"`
}
type natsRenderedUser struct {
	NKey        string `json:"nkey"`
	Permissions any    `json:"permissions"`
}
type natsAuthorization struct {
	Users []natsAuthorizedUser `json:"users"`
}

func fixedNATSPermissions(role string) natsPermissions {
	pub := []string{"$JS.API.STREAM.INFO." + platformStream}
	switch role {
	case "account-outbox-relay", "billing-outbox-relay", "token-outbox-relay":
		source := role[:len(role)-len("-outbox-relay")]
		pub = append(pub, source+".>", "account-platform.dead-letter."+source+".>")
	case "event-resource-operator":
		pub = append(pub, "$JS.API.STREAM.CREATE."+platformStream)
		pub = append(pub, "$JS.API.CONSUMER.NAMES."+platformStream)
		for _, consumer := range provisionedConsumers() {
			pub = append(pub, "$JS.API.CONSUMER.INFO."+platformStream+"."+consumer.Name, "$JS.API.CONSUMER.CREATE."+platformStream+"."+consumer.Name+"."+consumer.FilterSubject)
		}
		for _, descriptor := range approvalconsumer.BillingCommands() {
			pub = append(pub, "$JS.API.CONSUMER.INFO."+platformStream+"."+descriptor.Durable, "$JS.API.CONSUMER.CREATE."+platformStream+"."+descriptor.Durable+"."+approvalconsumer.AuthorizationSubject)
		}
	default:
		for _, consumer := range provisionedConsumers() {
			if !consumerOwnedBy(role, consumer) {
				continue
			}
			pub = append(pub, "$JS.API.CONSUMER.INFO."+platformStream+"."+consumer.Name, "$JS.API.CONSUMER.MSG.NEXT."+platformStream+"."+consumer.Name, "$JS.ACK."+platformStream+"."+consumer.Name+".>")
			if role == "billing-command-executor" {
				pub = append(pub, "account-platform.dead-letter."+consumer.FilterSubject)
			}
		}
	}
	return natsPermissions{Publish: pub, Subscribe: []string{"_INBOX." + role + ".>"}}
}

func fixedNATSV2Permissions(role string) any {
	const authorizationSubject = "account.approval-command.authorized.v1"
	const authorizationDeadLetter = "account-platform.dead-letter.account.approval-command.authorized.v1"
	if role == "account-approval-relay" {
		return struct {
			Publish   natsSubjectPermission `json:"publish"`
			Subscribe natsSubjectPermission `json:"subscribe"`
		}{
			Publish:   natsSubjectPermission{Allow: []string{authorizationSubject, authorizationDeadLetter}},
			Subscribe: natsSubjectPermission{Allow: []string{"_INBOX.account-approval-relay.>"}},
		}
	}
	if role == "account-outbox-relay" {
		return struct {
			Publish   natsSubjectPermission `json:"publish"`
			Subscribe natsSubjectPermission `json:"subscribe"`
		}{
			Publish:   natsSubjectPermission{Allow: []string{"$JS.API.STREAM.INFO." + platformStream, "account.>", "account-platform.dead-letter.account.>"}, Deny: []string{authorizationSubject, authorizationDeadLetter}},
			Subscribe: natsSubjectPermission{Allow: []string{"_INBOX.account-outbox-relay.>"}},
		}
	}
	return fixedNATSPermissions(role)
}

// RenderNATSAuthorization evaluates one trusted UTC instant, then delegates to
// a deterministic renderer. A previous artifact is accepted only during the
// bounded overlap encoded by v2.
func RenderNATSAuthorization(current, previous []byte) ([]byte, error) {
	return renderNATSAuthorizationAt(current, previous, time.Now().UTC().Truncate(time.Second))
}

func renderNATSAuthorizationAt(current, previous []byte, now time.Time) ([]byte, error) {
	if now.Location() != time.UTC {
		return nil, ErrNATSResources
	}
	primary, err := parseNATSIdentityArtifact(current)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	authorization := struct {
		Users []natsRenderedUser `json:"users"`
	}{}
	appendUser := func(public, role string, permissions any) error {
		if seen[public] {
			return ErrNATSResources
		}
		seen[public] = true
		authorization.Users = append(authorization.Users, natsRenderedUser{NKey: public, Permissions: permissions})
		return nil
	}
	if primary.version == 1 {
		for _, role := range fixedNATSRoleNames() {
			if role == "token-outbox-relay" || role == "token-command-executor" {
				continue
			}
			if err := appendUser(primary.keys[role], role, fixedNATSPermissions(role)); err != nil {
				return nil, err
			}
		}
		if len(previous) > 0 {
			old, err := parseNATSIdentityArtifact(previous)
			if err != nil || old.version != 1 {
				return nil, ErrNATSResources
			}
			for _, role := range fixedNATSRoleNames() {
				if role == "token-outbox-relay" || role == "token-command-executor" {
					continue
				}
				if err := appendUser(old.keys[role], role, fixedNATSPermissions(role)); err != nil {
					return nil, err
				}
			}
		}
	} else {
		var old natsIdentityArtifact
		if len(previous) > 0 {
			old, err = parseNATSIdentityArtifact(previous)
			digest := sha256.Sum256(previous)
			remaining := primary.overlapExpiresAt.Sub(now)
			if err != nil || old.version != 1 || hex.EncodeToString(digest[:]) != primary.previousV1Digest || remaining <= 0 || remaining > natsIdentityOverlapMaximum {
				return nil, ErrNATSResources
			}
			for _, role := range []string{"billing-outbox-relay", "token-outbox-relay", "billing-command-executor", "token-command-executor", "event-resource-operator"} {
				if primary.keys[role] != old.keys[role] {
					return nil, ErrNATSResources
				}
			}
			if primary.keys["account-outbox-relay"] == old.keys["account-outbox-relay"] || primary.keys["account-approval-relay"] == old.keys["account-outbox-relay"] {
				return nil, ErrNATSResources
			}
		}
		for _, role := range fixedNATSV2RoleNames() {
			if role == "token-outbox-relay" || role == "token-command-executor" {
				continue
			}
			if err := appendUser(primary.keys[role], role, fixedNATSV2Permissions(role)); err != nil {
				return nil, err
			}
		}
		if len(previous) > 0 {
			if err := appendUser(old.keys["account-outbox-relay"], "account-outbox-relay", fixedNATSPermissions("account-outbox-relay")); err != nil {
				return nil, err
			}
		}
	}
	var output bytes.Buffer
	d := json.NewEncoder(&output)
	d.SetEscapeHTML(false)
	if d.Encode(struct {
		Authorization any `json:"authorization"`
	}{authorization}) != nil {
		return nil, ErrNATSResources
	}
	return output.Bytes(), nil
}
