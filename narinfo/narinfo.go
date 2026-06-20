package narinfo

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
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
	References  []string // full /nix/store/... paths
	Deriver     string   // full /nix/store/... path or empty
	Sigs        []string // "name:base64" strings
	CA          string
}

const nixStorePrefix = "/nix/store/"

// Parse parses a narinfo from r.
func Parse(r io.Reader) (*NarInfo, error) {
	n := &NarInfo{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			// Allow lines like "References: " (no value after colon-space) by
			// re-trying with just ":"
			key, value, ok = strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimPrefix(value, " ")
		}
		switch key {
		case "StorePath":
			n.StorePath = value
		case "URL":
			n.URL = value
		case "Compression":
			n.Compression = value
		case "FileHash":
			n.FileHash = value
		case "FileSize":
			v, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("narinfo: invalid FileSize %q: %w", value, err)
			}
			n.FileSize = v
		case "NarHash":
			n.NarHash = value
		case "NarSize":
			v, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("narinfo: invalid NarSize %q: %w", value, err)
			}
			n.NarSize = v
		case "References":
			if value == "" {
				n.References = []string{}
				continue
			}
			// Parse basenames in-place to avoid the intermediate []string from Fields.
			refs := make([]string, 0, strings.Count(value, " ")+1)
			for value != "" {
				var basename string
				if idx := strings.IndexByte(value, ' '); idx >= 0 {
					basename, value = value[:idx], value[idx+1:]
				} else {
					basename, value = value, ""
				}
				if basename != "" {
					refs = append(refs, nixStorePrefix+basename)
				}
			}
			n.References = refs
		case "Deriver":
			if value != "" {
				n.Deriver = nixStorePrefix + value
			}
		case "Sig":
			n.Sigs = append(n.Sigs, value)
		case "CA":
			n.CA = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("narinfo: scan error: %w", err)
	}
	return n, nil
}

// Marshal serializes n to narinfo text format in the exact harmonia field order.
func (n *NarInfo) Marshal() string {
	// Estimate output size to avoid builder re-allocations.
	size := 11 + len(n.StorePath) + 1 +
		6 + len(n.URL) + 1 +
		14 + len(n.Compression) + 1 +
		11 + len(n.FileHash) + 1 +
		11 + 20 + 1 + // FileSize (up to 20 digits)
		10 + len(n.NarHash) + 1 +
		10 + 20 + 1 + // NarSize
		13 + 1 // "References: \n"
	for _, ref := range n.References {
		size += len(ref) - len(nixStorePrefix) + 1 // basename + space
	}
	for _, sig := range n.Sigs {
		size += 6 + len(sig) + 1
	}
	if n.Deriver != "" {
		size += 9 + len(n.Deriver) - len(nixStorePrefix) + 1
	}
	if n.CA != "" {
		size += 4 + len(n.CA) + 1
	}

	var b strings.Builder
	b.Grow(size)

	b.WriteString("StorePath: ")
	b.WriteString(n.StorePath)
	b.WriteByte('\n')

	b.WriteString("URL: ")
	b.WriteString(n.URL)
	b.WriteByte('\n')

	b.WriteString("Compression: ")
	b.WriteString(n.Compression)
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
	b.WriteString("References: ")
	for i, ref := range n.References {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strings.TrimPrefix(ref, nixStorePrefix))
	}
	b.WriteByte('\n')

	if n.Deriver != "" {
		b.WriteString("Deriver: ")
		b.WriteString(strings.TrimPrefix(n.Deriver, nixStorePrefix))
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
