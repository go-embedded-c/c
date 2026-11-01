// Package golden reads and writes the golden files: what the oracle did with
// each program, as text that a diff shows.
//
// Format (version 1), one record per program:
//
//	@ name
//	exit 3
//	trap "unreachable"        (omitted when the run did not trap)
//	stdout "hi 4\n"
//	stderr ""
//
// Strings are Go-quoted. The decoder is strict and bounded: it is fed files
// from a working tree, and fuzzed.
package golden

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Header is the first line of every golden file.
const Header = "# coracle golden v1"

// Limits on what Decode accepts.
const (
	MaxLine    = 8 << 20 // bytes in one line (a quoted stream)
	MaxRecords = 1 << 20
)

// Record is the oracle's answer for one program.
type Record struct {
	Name   string
	Exit   uint32
	Trap   string
	Stdout []byte
	Stderr []byte
}

// Encode writes recs in the golden format.
func Encode(w io.Writer, recs []Record) error {
	for _, r := range recs {
		if r.Name == "" || strings.ContainsAny(r.Name, "\r\n") {
			return fmt.Errorf("golden: bad record name %q", r.Name)
		}
	}
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, Header)
	for _, r := range recs {
		fmt.Fprintf(bw, "@ %s\n", r.Name)
		fmt.Fprintf(bw, "exit %d\n", r.Exit)
		if r.Trap != "" {
			fmt.Fprintf(bw, "trap %s\n", strconv.Quote(r.Trap))
		}
		fmt.Fprintf(bw, "stdout %s\n", strconv.Quote(string(r.Stdout)))
		fmt.Fprintf(bw, "stderr %s\n", strconv.Quote(string(r.Stderr)))
	}
	return bw.Flush()
}

// Decode parses a golden file. It rejects a missing header, an unknown or
// out-of-order line, a malformed number or string, a record without its
// fields, and a name that is empty or holds a newline.
func Decode(r io.Reader) ([]Record, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLine)
	line := 0
	next := func() (string, bool) {
		if !sc.Scan() {
			return "", false
		}
		line++
		return sc.Text(), true
	}
	fail := func(format string, a ...any) error {
		return fmt.Errorf("golden: line %d: %s", line, fmt.Sprintf(format, a...))
	}
	if h, ok := next(); !ok || h != Header {
		if err := sc.Err(); err != nil {
			return nil, err
		}
		return nil, fail("missing header %q", Header)
	}
	var recs []Record
	for {
		l, ok := next()
		if !ok {
			break
		}
		name, isRec := strings.CutPrefix(l, "@ ")
		if !isRec || name == "" {
			return nil, fail("expected \"@ name\", got %q", abbreviate(l))
		}
		if len(recs) >= MaxRecords {
			return nil, fail("more than %d records", MaxRecords)
		}
		rec := Record{Name: name}
		l, ok = next()
		v, isExit := strings.CutPrefix(l, "exit ")
		if !ok || !isExit {
			return nil, fail("expected \"exit N\"")
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return nil, fail("bad exit status %q", abbreviate(v))
		}
		rec.Exit = uint32(n)
		l, ok = next()
		if t, isTrap := strings.CutPrefix(l, "trap "); ok && isTrap {
			if rec.Trap, err = strconv.Unquote(t); err != nil || rec.Trap == "" {
				return nil, fail("bad trap string")
			}
			l, ok = next()
		}
		for _, f := range []struct {
			key string
			dst *[]byte
		}{{"stdout ", &rec.Stdout}, {"stderr ", &rec.Stderr}} {
			q, has := strings.CutPrefix(l, f.key)
			if !ok || !has {
				return nil, fail("expected %q", strings.TrimSpace(f.key))
			}
			s, err := strconv.Unquote(q)
			if err != nil {
				return nil, fail("bad %s string", strings.TrimSpace(f.key))
			}
			*f.dst = []byte(s)
			if f.key == "stdout " {
				l, ok = next()
			}
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("golden: %w", err)
	}
	return recs, nil
}

func abbreviate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// Equal reports whether two records say the same.
func (r Record) Equal(o Record) bool {
	return r.Name == o.Name && r.Exit == o.Exit && r.Trap == o.Trap &&
		bytes.Equal(r.Stdout, o.Stdout) && bytes.Equal(r.Stderr, o.Stderr)
}
