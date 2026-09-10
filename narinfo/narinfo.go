// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package narinfo parses and renders the Nix .narinfo metadata format.
package narinfo

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/kradalby/tsnixcache/nixbase32"
)

// NarInfo represents a parsed .narinfo file.
type NarInfo struct {
	StorePath   string
	URL         string
	Compression string
	FileHash    string
	FileSize    uint64
	NarHash     string
	NarSize     uint64
	References  []string // full store paths (store dir + basename)
	Deriver     string   // full store path or empty
	Sigs        []string // "name:base64" strings
	CA          string
}

// DefaultStoreDir is the store directory assumed when Parse is given an empty
// one. The server's --store-dir may differ, hence the parameter.
const DefaultStoreDir = "/nix/store"

// defaultCompression is what an omitted Compression field means. It is nix's
// default, not this cache's: a narinfo without the field describes a bzip2 NAR,
// so an empty value must never be treated — or emitted — as "no compression".
const defaultCompression = "bzip2"

// unknownDeriver is the sentinel nix writes for a path whose deriver it does not
// know. Taken literally it names a store path that does not exist.
const unknownDeriver = "unknown-deriver"

// maxNameLen is nix's StorePath::MaxPathLen: the longest name part a store path
// basename may carry.
const maxNameLen = 211

var (
	// errTooManyReferences is returned for a References line carrying more
	// entries than its length could hold.
	errTooManyReferences = errors.New("narinfo: too many References for the length of the line")

	// errTooManySigs bounds repeated signature metadata.
	errTooManySigs = errors.New("narinfo: too many Sig lines for the length of the narinfo")

	// errMissingField is returned when a field nix requires is absent or empty.
	errMissingField = errors.New("narinfo: missing required field")

	// errInvalidStorePath is returned for a StorePath that is not a path in the
	// store this narinfo was parsed against.
	errInvalidStorePath = errors.New("narinfo: invalid StorePath")

	// errInvalidName is returned for a reference or Deriver that is not a bare
	// store path basename.
	errInvalidName = errors.New("narinfo: invalid store path basename")

	// errInvalidHash is returned for a hash in no encoding nix would accept.
	errInvalidHash = errors.New("narinfo: unparseable hash")

	// errDuplicateField is returned for a repeat of a field that may appear once.
	errDuplicateField = errors.New("narinfo: duplicate field")
)

// storePrefix returns storeDir with a trailing slash, ready to prepend to a
// basename.
func storePrefix(storeDir string) string {
	if storeDir == "" {
		storeDir = DefaultStoreDir
	}

	return strings.TrimSuffix(storeDir, "/") + "/"
}

// validName reports whether s is a well-formed store path name, matching nix's
// checkName. The alphabet excludes "/" and NUL and a leading dot is refused, so
// a name that passes can only ever be a single entry inside the directory it is
// joined to — which is what keeps a pushed narinfo from naming files outside the
// store or the GC root directory.
func validName(s string) bool {
	if s == "" || len(s) > maxNameLen || s[0] == '.' {
		return false
	}

	for i := range len(s) {
		c := s[i]

		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '+', c == '-', c == '.', c == '_', c == '?', c == '=':
		default:
			return false
		}
	}

	return true
}

// validBasename reports whether s is a store path basename: a 32-character
// nix-base32 hash part, a dash and a name. The hash alphabet has no dash, so
// cutting at the first one always splits the two apart.
func validBasename(s string) bool {
	hash, name, ok := strings.Cut(s, "-")

	return ok && nixbase32.ValidHashPart(hash) && validName(name)
}

// hashSizes maps a hash algorithm to its digest length in bytes.
var hashSizes = map[string]int{"md5": 16, "sha1": 20, "sha256": 32, "sha512": 64}

// CanonicalHash renders a narinfo hash the way nix does, as "<algo>:<nix32>".
//
// Colon-prefixed hashes select an encoding by length; SRI always uses base64.
// Normalize aliases before comparing hashes or constructing fingerprints.
func CanonicalHash(s string) (string, error) {
	algo, digest, colon := strings.Cut(s, ":")
	if !colon {
		var ok bool

		algo, digest, ok = strings.Cut(s, "-")
		if !ok {
			return "", fmt.Errorf("%w: %q has no algorithm prefix", errInvalidHash, s)
		}
	}

	size, ok := hashSizes[algo]
	if !ok {
		return "", fmt.Errorf("%w: unknown algorithm in %q", errInvalidHash, s)
	}

	var (
		raw []byte
		err error
	)
	if colon {
		raw, err = decodeDigest(digest, size)
	} else {
		raw, err = decodeBase64(digest)
	}

	if err != nil {
		return "", fmt.Errorf("%w %q: %w", errInvalidHash, s, err)
	}

	if len(raw) != size {
		return "", fmt.Errorf("%w: %s requires %d bytes, got %d", errInvalidHash, algo, size, len(raw))
	}

	return algo + ":" + nixbase32.EncodeToString(raw), nil
}

func decodeDigest(s string, size int) ([]byte, error) {
	switch len(s) {
	case hex.EncodedLen(size):
		return hex.DecodeString(s)
	case (size*8 + 4) / 5:
		return nixbase32.DecodeString(s)
	case base64.StdEncoding.EncodedLen(size):
		return decodeBase64(s)
	}

	return nil, fmt.Errorf("%w: %d digest characters", errInvalidHash, len(s))
}

func decodeBase64(s string) ([]byte, error) {
	// Nix accepts missing padding and stops at the first padding character.
	s, _, _ = strings.Cut(s, "=")
	// Go ignores CR as well as LF; Nix ignores only LF.
	if strings.ContainsRune(s, '\r') {
		return nil, errInvalidHash
	}

	return base64.RawStdEncoding.DecodeString(s)
}

// maxReferences is a conservative allocation ceiling for valid store basenames.
func maxReferences(length int) int {
	return length/33 + 1
}

// Bound repeated metadata independently of ignored input fields.
const maxSignatures = 1024

// parser tracks the store prefix and single-occurrence fields.
type parser struct {
	n        NarInfo
	prefix   string
	seenRefs bool
	seenCA   bool
}

// parseReferences validates basenames before allocating their store prefixes.
// Bound the slice by input length; malformed tokens must not amplify allocation.
func (p *parser) parseReferences(value string) error {
	if value == "" {
		p.n.References = []string{}

		return nil
	}

	// Parse basenames in-place to avoid the intermediate []string from Fields.
	maxRefs := maxReferences(len(value))
	refs := make([]string, 0, maxRefs)

	for value != "" {
		var basename string

		basename, value, _ = strings.Cut(value, " ")
		if basename == "" {
			continue
		}

		// An entry carrying its own store dir would be prefixed a second time,
		// giving "/nix/store//nix/store/…" in the export stream and the
		// fingerprint while Marshal's path.Base quietly collapses it back, so
		// parse and marshal disagree about the same narinfo.
		if !validBasename(basename) {
			return fmt.Errorf("%w: reference %q", errInvalidName, basename)
		}

		if len(refs) == maxRefs {
			return fmt.Errorf("%w: at most %d", errTooManyReferences, maxRefs)
		}

		refs = append(refs, p.prefix+basename)
	}

	p.n.References = refs

	return nil
}

// setStorePath confines metadata to the configured store.
func (p *parser) setStorePath(value string) error {
	base, ok := strings.CutPrefix(value, p.prefix)
	if !ok || !validBasename(base) {
		return fmt.Errorf("%w: %q", errInvalidStorePath, value)
	}

	p.n.StorePath = value

	return nil
}

// setDeriver records Deriver, treating nix's "unknown-deriver" sentinel as the
// absence it means rather than as a store path of that name.
func (p *parser) setDeriver(value string) error {
	if value == "" || value == unknownDeriver {
		return nil
	}

	if !validBasename(value) {
		return fmt.Errorf("%w: Deriver %q", errInvalidName, value)
	}

	p.n.Deriver = p.prefix + value

	return nil
}

// addSig appends a signature, refusing more than the narinfo read so far could
// plausibly carry.
func (p *parser) addSig(value string) error {
	if len(p.n.Sigs) >= maxSignatures {
		return fmt.Errorf("%w: at most %d", errTooManySigs, maxSignatures)
	}

	p.n.Sigs = append(p.n.Sigs, value)

	return nil
}

// parseNumericFields handles the two numeric fields (FileSize, NarSize) and
// returns an error on invalid input.
func (p *parser) parseNumericFields(key, value string) error {
	v, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return fmt.Errorf("narinfo: invalid %s %q: %w", key, value, err)
	}

	if key == "FileSize" {
		p.n.FileSize = v
	} else {
		p.n.NarSize = v
	}

	return nil
}

// parseHashFields normalises FileHash and NarHash to nix's canonical form.
func (p *parser) parseHashFields(key, value string) error {
	h, err := CanonicalHash(value)
	if err != nil {
		return fmt.Errorf("narinfo: %s: %w", key, err)
	}

	if key == "FileHash" {
		p.n.FileHash = h
	} else {
		p.n.NarHash = h
	}

	return nil
}

// parseField applies a single key/value pair, returning an error for a value
// the field cannot hold.
func (p *parser) parseField(key, value string) error {
	switch key {
	case "StorePath":
		return p.setStorePath(value)
	case "URL":
		p.n.URL = value
	case "Compression":
		p.n.Compression = value
	case "FileHash", "NarHash":
		return p.parseHashFields(key, value)
	case "FileSize", "NarSize":
		return p.parseNumericFields(key, value)
	case "References":
		if p.seenRefs {
			return fmt.Errorf("%w: References", errDuplicateField)
		}

		p.seenRefs = true

		return p.parseReferences(value)
	case "Deriver":
		return p.setDeriver(value)
	case "Sig":
		return p.addSig(value)
	case "CA":
		if p.seenCA {
			return fmt.Errorf("%w: CA", errDuplicateField)
		}

		p.seenCA = true
		p.n.CA = value
	}

	return nil
}

// parseLine applies one "Key: value" line; blank and unrecognised lines are
// ignored.
func (p *parser) parseLine(line string) error {
	if line == "" {
		return nil
	}

	key, value, ok := strings.Cut(line, ": ")
	if !ok {
		// Allow lines like "References:" (no value after colon-space) by
		// re-trying with just ":"
		key, value, ok = strings.Cut(line, ":")
		if !ok {
			return nil
		}

		value = strings.TrimPrefix(value, " ")
	}

	return p.parseField(key, value)
}

// finish applies defaults and rejects incomplete metadata before import.
func (p *parser) finish() (*NarInfo, error) {
	switch {
	case p.n.StorePath == "":
		return nil, fmt.Errorf("%w: StorePath", errMissingField)
	case p.n.NarHash == "":
		return nil, fmt.Errorf("%w: NarHash", errMissingField)
	case p.n.URL == "":
		return nil, fmt.Errorf("%w: URL", errMissingField)
	case p.n.NarSize == 0:
		return nil, fmt.Errorf("%w: NarSize", errMissingField)
	}

	if p.n.Compression == "" {
		p.n.Compression = defaultCompression
	}

	return &p.n, nil
}

// Parse parses a narinfo from r. storeDir is the store directory the StorePath,
// References and Deriver belong to; empty means DefaultStoreDir.
//
// Lines are read with a bufio.Reader rather than a bufio.Scanner because a
// large closure's References line runs to megabytes and Scanner rejects
// anything over 64 KiB, turning a perfectly valid narinfo into a parse error.
// Total size is bounded by the caller (the server caps PUT bodies).
// At most 1024 signature fields are accepted.
func Parse(r io.Reader, storeDir string) (*NarInfo, error) {
	p := parser{prefix: storePrefix(storeDir)}
	br := bufio.NewReader(r)

	for {
		line, readErr := br.ReadString('\n')

		// Accept both LF and CRLF line endings.
		err := p.parseLine(strings.TrimRight(line, "\r\n"))
		if err != nil {
			return nil, err
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return p.finish()
			}

			return nil, fmt.Errorf("narinfo: read error: %w", readErr)
		}
	}
}

// Marshal serialises n to narinfo text format in the exact harmonia field order.
//
// References and Deriver are emitted as bare basenames, which is what nix
// expects regardless of the store directory — hence path.Base rather than
// trimming a fixed prefix, which silently emitted absolute paths whenever the
// store dir was not /nix/store.
func (n *NarInfo) Marshal() string {
	var b strings.Builder

	b.Grow(n.marshalSize())

	b.WriteString("StorePath: ")
	b.WriteString(n.StorePath)
	b.WriteByte('\n')

	b.WriteString("URL: ")
	b.WriteString(n.URL)
	b.WriteByte('\n')

	b.WriteString("Compression: ")
	b.WriteString(n.compression())
	b.WriteByte('\n')

	if n.FileHash != "" {
		b.WriteString("FileHash: ")
		b.WriteString(n.FileHash)
		b.WriteByte('\n')
	}

	b.WriteString("FileSize: ")
	b.WriteString(strconv.FormatUint(n.FileSize, 10))
	b.WriteByte('\n')

	b.WriteString("NarHash: ")
	b.WriteString(n.NarHash)
	b.WriteByte('\n')

	b.WriteString("NarSize: ")
	b.WriteString(strconv.FormatUint(n.NarSize, 10))
	b.WriteByte('\n')

	// References: always emitted (even if empty), as space-separated basenames.
	// The trailing space matters: nix reads the value from colon+2, so a
	// reference-free path whose line is "References:" is unreadable to it.
	b.WriteString("References: ")

	for i, ref := range n.References {
		if i > 0 {
			b.WriteByte(' ')
		}

		b.WriteString(path.Base(ref))
	}

	b.WriteByte('\n')

	if n.Deriver != "" {
		b.WriteString("Deriver: ")
		b.WriteString(path.Base(n.Deriver))
		b.WriteByte('\n')
	}

	for _, sig := range n.Sigs {
		b.WriteString("Sig: ")
		b.WriteString(sig)
		b.WriteByte('\n')
	}

	if n.CA != "" {
		b.WriteString("CA: ")
		b.WriteString(n.CA)
		b.WriteByte('\n')
	}

	return b.String()
}

// Fingerprint returns the signing fingerprint string.
func (n *NarInfo) Fingerprint() string {
	refs := n.References
	if !sort.StringsAreSorted(refs) {
		cp := make([]string, len(refs))
		copy(cp, refs)
		sort.Strings(cp)
		refs = cp
	}

	// Pre-size: "1;" + storePath + ";" + narHash + ";" + narSize + ";" + refs
	size := 2 + len(n.StorePath) + 1 + len(n.NarHash) + 1 + 20 + 1

	for i, r := range refs {
		size += len(r)
		if i > 0 {
			size++ // comma
		}
	}

	var b strings.Builder

	b.Grow(size)
	b.WriteString("1;")
	b.WriteString(n.StorePath)
	b.WriteByte(';')
	b.WriteString(n.NarHash)
	b.WriteByte(';')
	b.WriteString(strconv.FormatUint(n.NarSize, 10))
	b.WriteByte(';')

	for i, r := range refs {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteString(r)
	}

	return b.String()
}

// compression is the value Marshal writes. An empty one must never be emitted
// bare: nix reads "Compression: " back as bzip2, not as "none", so a zero value
// would silently redescribe an uncompressed NAR. Parse applies the same default,
// so the two directions agree.
func (n *NarInfo) compression() string {
	if n.Compression == "" {
		return defaultCompression
	}

	return n.Compression
}

// marshalSize is the length of Marshal's output, used as the builder's Grow
// hint. It is exact except for the two numeric fields, which are budgeted at
// the 20 digits of a maximal uint64. References and Deriver are measured as
// the basenames Marshal actually writes, not the full store paths.
func (n *NarInfo) marshalSize() int {
	const maxUint64Digits = 20

	size := len("StorePath: ") + len(n.StorePath) + 1 +
		len("URL: ") + len(n.URL) + 1 +
		len("Compression: ") + len(n.compression()) + 1 +
		len("FileSize: ") + maxUint64Digits + 1 +
		len("NarHash: ") + len(n.NarHash) + 1 +
		len("NarSize: ") + maxUint64Digits + 1 +
		len("References: ") + 1

	if n.FileHash != "" {
		size += len("FileHash: ") + len(n.FileHash) + 1
	}

	for i, ref := range n.References {
		if i > 0 {
			size++ // separating space
		}

		size += len(path.Base(ref))
	}

	for _, sig := range n.Sigs {
		size += len("Sig: ") + len(sig) + 1
	}

	if n.Deriver != "" {
		size += len("Deriver: ") + len(path.Base(n.Deriver)) + 1
	}

	if n.CA != "" {
		size += len("CA: ") + len(n.CA) + 1
	}

	return size
}
