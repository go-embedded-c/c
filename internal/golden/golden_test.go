package golden

import (
	"bytes"
	"strings"
	"testing"
)

func sample() []Record {
	return []Record{
		{Name: "a", Exit: 0, Stdout: []byte("hi\n")},
		{Name: "b/c d", Exit: 4294967295, Trap: "wasm error: unreachable", Stdout: []byte{0, 1, 0xff, '"'}, Stderr: []byte("e\r\n")},
	}
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, sample()); err != nil {
		t.Fatal(err)
	}
	got, err := Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	want := sample()
	if len(got) != len(want) {
		t.Fatalf("%d records, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("record %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestEncodeRefusesBadNames(t *testing.T) {
	for _, n := range []string{"", "a\nb", "a\rb"} {
		if err := Encode(&bytes.Buffer{}, []Record{{Name: n}}); err == nil {
			t.Errorf("name %q accepted", n)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	ok := Header + "\n@ n\nexit 0\nstdout \"\"\nstderr \"\"\n"
	if _, err := Decode(strings.NewReader(ok)); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"empty":           "",
		"no header":       "@ n\n",
		"not a record":    Header + "\nfoo\n",
		"empty name":      Header + "\n@ \n",
		"no exit":         Header + "\n@ n\nstdout \"\"\n",
		"exit eof":        Header + "\n@ n\n",
		"bad exit":        Header + "\n@ n\nexit x\n",
		"exit too big":    Header + "\n@ n\nexit 4294967296\n",
		"bad trap":        Header + "\n@ n\nexit 0\ntrap x\n",
		"empty trap":      Header + "\n@ n\nexit 0\ntrap \"\"\n",
		"no stdout":       Header + "\n@ n\nexit 0\nstderr \"\"\n",
		"bad stdout":      Header + "\n@ n\nexit 0\nstdout x\nstderr \"\"\n",
		"no stderr":       Header + "\n@ n\nexit 0\nstdout \"\"\n",
		"bad stderr":      Header + "\n@ n\nexit 0\nstdout \"\"\nstderr x\n",
		"trap then eof":   Header + "\n@ n\nexit 0\ntrap \"t\"\n",
		"line over limit": Header + "\n@ n\nexit 0\nstdout \"" + strings.Repeat("a", MaxLine) + "\"\n",
	} {
		if _, err := Decode(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecodeRecordLimit(t *testing.T) {
	rec := "@ n\nexit 0\nstdout \"\"\nstderr \"\"\n"
	in := Header + "\n" + strings.Repeat(rec, MaxRecords+1)
	if _, err := Decode(strings.NewReader(in)); err == nil || !strings.Contains(err.Error(), "records") {
		t.Fatalf("%v", err)
	}
}

func TestAbbreviate(t *testing.T) {
	if got := abbreviate(strings.Repeat("x", 100)); len(got) != 43 {
		t.Fatal(len(got))
	}
}

// FuzzDecode: the decoder is fed files from a working tree. It must neither
// panic nor hang, and what it accepts must survive a round trip.
func FuzzDecode(f *testing.F) {
	var buf bytes.Buffer
	_ = Encode(&buf, sample())
	f.Add(buf.Bytes())
	f.Add([]byte(Header + "\n@ n\nexit 0\ntrap \"x\"\nstdout \"\\x00\"\nstderr \"\"\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		recs, err := Decode(bytes.NewReader(data))
		if err != nil {
			return
		}
		var out bytes.Buffer
		if err := Encode(&out, recs); err != nil {
			return // a name with an inner \r is decodable but not encodable
		}
		again, err := Decode(&out)
		if err != nil || len(again) != len(recs) {
			t.Fatalf("round trip lost records: %v", err)
		}
		for i := range recs {
			if !again[i].Equal(recs[i]) {
				t.Fatalf("record %d changed: %+v vs %+v", i, again[i], recs[i])
			}
		}
	})
}
