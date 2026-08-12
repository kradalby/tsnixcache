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

	// errTooManySigs is returned for a narinfo carrying more Sig lines than its
	// length could hold.
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
// Nix accepts the digest in base16, nix-base32 or base64 and takes either ":"
// or the SRI "-" as the separator, then always prints it back in nix-base32.
// Storing whatever form arrived instead means a correct hash gets string-
// compared against a re-encoding of itself: a hex NarHash on byte-identical
// content is reported as a mismatch, and a fingerprint built from it is one nix
// will never reproduce.
func CanonicalHash(s string) (string, error) {
	i := strings.IndexAny(s, ":-")
	if i < 0 {
		return "", fmt.Errorf("%w: %q has no algorithm prefix", errInvalidHash, s)
	}

	algo, digest := s[:i], s[i+1:]

	size, ok := hashSizes[algo]
	if !ok {
		return "", fmt.Errorf("%w: unknown algorithm in %q", errInvalidHash, s)
	}

	raw, err := decodeDigest(digest, size)
	if err != nil {
		return "", fmt.Errorf("%w %q: %w", errInvalidHash, s, err)
	}

	return algo + ":" + nixbase32.EncodeToString(raw), nil
}

// decodeDigest decodes a size-byte digest, picking the encoding by length as
// nix's Hash::parseAnyPrefixed does. The three lengths are distinct for every
// algorithm nix supports, so the length alone identifies the encoding.
func decodeDigest(s string, size int) ([]byte, error) {
	switch len(s) {
	case hex.EncodedLen(size):
		return hex.DecodeString(s)
	case (size*8 + 4) / 5:
		return nixbase32.DecodeString(s)
	case base64.StdEncoding.EncodedLen(size):
		return base64.StdEncoding.DecodeString(s)
	}

	return nil, fmt.Errorf("%w: %d digest characters", errInvalidHash, len(s))
}

// maxReferences is the most entries a References line of length bytes can hold.
// A store path basename is a 32-character hash, a dash and a name, so n of them
// separated by single spaces need at least 33n bytes; the +1 covers a line
// holding a single basename.
func maxReferences(length int) int {
	return length/33 + 1
}

// maxSigs is the most Sig lines a narinfo of length bytes can hold. A signature
// line is "Sig: ", a key name, a colon and a base64 ed25519 signature of 88
// characters, so 95 bytes is the shortest one that could carry a signature at
// all; the +1 covers a narinfo holding a single Sig.
//
// Sig was the last repeated field with no ceiling: a body of bare "Sig: " lines
// bought roughly four bytes of slice per byte of request.
func maxSigs(length int) int {
	return length/95 + 1
}

// parser accumulates a narinfo as its lines arrive. It holds the state the
// per-line checks need: the store prefix basenames resolve against, the bytes
// consumed so far (which bounds the repeated fields) and which
// single-occurrence fields have already been seen.
type parser struct {
	n        NarInfo
	prefix   string
	consumed int
	seenRefs bool
	seenCA   bool
}

// parseReferences parses a space-separated list of store path basenames into
// full store paths under the store prefix.
//
// The line length bounds the entry count twice over: it pre-sizes the slice,
// and it is a hard ceiling. Both halves are needed. Sizing alone bounds nothing
// — a 1 MiB line of one-character tokens under-counts by a factor of 17 and
// append then grows past the estimate by doubling, allocating ~56 MB for a
// body the server caps at 1 MiB. With the ceiling the slice never grows, so a
// line of any shape costs at most one header and one prefixed string per 33
// bytes, roughly twice the line. A line over the ceiling cannot be a list of
// store path basenames whatever it is, so reject it rather than truncate.
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

// setStorePath records StorePath after checking it is a path in this store.
// Nothing between here and the importer re-checks it, and the importer turns it
// into a GC root symlink, so an unchecked one lets a pushed narinfo name any
// file it likes.
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
	if maxSig := maxSigs(p.consumed); len(p.n.Sigs) >= maxSig {
		return fmt.Errorf("%w: at most %d", errTooManySigs, maxSig)
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

// finish applies the format's defaults and rejects a narinfo missing anything
// nix's NarInfo constructor requires.
//
// Without the check, "", "hello world\n" and a handful of random bytes all
// parse to a zero NarInfo with a nil error and the server treats them as a real
// push. An empty URL is the worst of them: the importer resolves the spool file
// to filepath.Join(SpoolDir, ".") — the spool directory itself — and then
// removes it.
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
// Total size is bounded by the caller (the server caps PUT bodies), not here.
func Parse(r io.Reader, storeDir string) (*NarInfo, error) {
	p := parser{prefix: storePrefix(storeDir)}
	br := bufio.NewReader(r)

	for {
		line, readErr := br.ReadString('\n')
		p.consumed += len(line)

		// TrimRight, not TrimSuffix: bufio.Scanner used to drop a trailing \r
		// along with the \n, and a CRLF narinfo must keep parsing.
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

	b.WriteString("FileHash: ")
	b.WriteString(n.FileHash)
	b.WriteByte('\n')

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
		len("FileHash: ") + len(n.FileHash) + 1 +
		len("FileSize: ") + maxUint64Digits + 1 +
		len("NarHash: ") + len(n.NarHash) + 1 +
		len("NarSize: ") + maxUint64Digits + 1 +
		len("References: ") + 1

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
